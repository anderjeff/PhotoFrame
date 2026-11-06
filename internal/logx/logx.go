// Package logx sets up structured logging that goes both to stdout (picked up
// by journald on the Pi and `docker logs` on the NAS) and to an in-memory ring
// buffer that is served over HTTP so logs can be read from the server UI.
package logx

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"photoframe/internal/api"
)

// New returns a logger writing text to stdout and entries into a ring of ringSize.
func New(level slog.Level, ringSize int) (*slog.Logger, *Ring) {
	ring := NewRing(ringSize)
	text := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	return slog.New(multiHandler{text, &ringHandler{ring: ring, level: level}}), ring
}

// ParseLevel parses "debug", "info", "warn", "error"; returns def on failure.
func ParseLevel(s string, def slog.Level) slog.Level {
	var l slog.Level
	if s == "" || l.UnmarshalText([]byte(s)) != nil {
		return def
	}
	return l
}

// Entry is one captured log record.
type Entry struct {
	Seq   uint64         `json:"seq"`
	Time  time.Time      `json:"time"`
	Level string         `json:"level"`
	Msg   string         `json:"msg"`
	Attrs map[string]any `json:"attrs,omitempty"`
	lvl   slog.Level
}

// Ring is a fixed-size, concurrency-safe buffer of the most recent entries.
type Ring struct {
	mu       sync.Mutex
	buf      []Entry
	start, n int
	seq      uint64
}

func NewRing(size int) *Ring { return &Ring{buf: make([]Entry, max(size, 1))} }

func (r *Ring) add(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	e.Seq = r.seq
	r.buf[(r.start+r.n)%len(r.buf)] = e
	if r.n < len(r.buf) {
		r.n++
	} else {
		r.start = (r.start + 1) % len(r.buf)
	}
}

// Since returns entries with Seq > seq and level >= minLevel (at most limit,
// newest kept), plus the latest sequence number for incremental polling.
func (r *Ring) Since(seq uint64, minLevel slog.Level, limit int) ([]Entry, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []Entry{}
	for i := 0; i < r.n; i++ {
		e := r.buf[(r.start+i)%len(r.buf)]
		if e.Seq > seq && e.lvl >= minLevel {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, r.seq
}

// ServeHTTP serves GET ?since=N&level=info&limit=500 as {"entries":[...],"next":N}.
// If the process restarted, next may be lower than the client's since; clients
// should then reset to 0.
func (r *Ring) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	since, _ := strconv.ParseUint(q.Get("since"), 10, 64)
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit <= 0 {
		limit = 1000
	}
	entries, next := r.Since(since, ParseLevel(q.Get("level"), slog.LevelDebug), limit)
	api.WriteJSON(w, http.StatusOK, map[string]any{"entries": entries, "next": next})
}

type ringHandler struct {
	ring   *Ring
	level  slog.Level
	prefix string         // group prefix, e.g. "req."
	attrs  map[string]any // pre-bound attrs from WithAttrs
}

func (h *ringHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *ringHandler) Handle(_ context.Context, r slog.Record) error {
	e := Entry{Time: r.Time, Level: r.Level.String(), Msg: r.Message, lvl: r.Level}
	if len(h.attrs) > 0 || r.NumAttrs() > 0 {
		e.Attrs = make(map[string]any, len(h.attrs)+r.NumAttrs())
		for k, v := range h.attrs {
			e.Attrs[k] = v
		}
		r.Attrs(func(a slog.Attr) bool { addAttr(e.Attrs, h.prefix, a); return true })
	}
	h.ring.add(e)
	return nil
}

func (h *ringHandler) WithAttrs(as []slog.Attr) slog.Handler {
	h2 := *h
	h2.attrs = make(map[string]any, len(h.attrs)+len(as))
	for k, v := range h.attrs {
		h2.attrs[k] = v
	}
	for _, a := range as {
		addAttr(h2.attrs, h.prefix, a)
	}
	return &h2
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := *h
	h2.prefix = h.prefix + name + "."
	return &h2
}

func addAttr(dst map[string]any, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, sub := range a.Value.Group() {
			addAttr(dst, p, sub)
		}
		return
	}
	var v any
	switch a.Value.Kind() {
	case slog.KindBool:
		v = a.Value.Bool()
	case slog.KindInt64:
		v = a.Value.Int64()
	case slog.KindUint64:
		v = a.Value.Uint64()
	case slog.KindFloat64:
		v = a.Value.Float64()
	default:
		v = a.Value.String() // errors, durations, times, structs → readable strings
	}
	dst[prefix+a.Key] = v
}

type multiHandler []slog.Handler

func (m multiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r.Clone())
		}
	}
	return nil
}

func (m multiHandler) WithAttrs(as []slog.Attr) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithAttrs(as)
	}
	return out
}

func (m multiHandler) WithGroup(name string) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithGroup(name)
	}
	return out
}

// Middleware logs each request at debug level (enable with LOG_LEVEL=debug).
func Middleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !log.Enabled(r.Context(), slog.LevelDebug) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if strings.HasSuffix(r.URL.Path, "/logs") || strings.HasSuffix(r.URL.Path, "/events") {
			return // avoid log noise from the log viewer itself
		}
		log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"dur", time.Since(start).Round(time.Microsecond), "remote", r.RemoteAddr)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(c int) { s.status = c; s.ResponseWriter.WriteHeader(c) }

// Unwrap lets http.ResponseController reach Flush on the underlying writer (needed for SSE).
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
