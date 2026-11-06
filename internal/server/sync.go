package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"photoframe/internal/api"
)

const pushConcurrency = 3

// FrameClient talks to the frame's HTTP API.
type FrameClient struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// Do sends a request to the frame. The caller must close the response body.
func (c *FrameClient) Do(ctx context.Context, method, path string, body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return nil, err
	}
	if size >= 0 && body != nil {
		req.ContentLength = size
	}
	req.Header.Set(api.TokenHeader, c.Token)
	return c.HTTP.Do(req)
}

func (c *FrameClient) call(ctx context.Context, method, path string, body io.Reader, size int64, out any) error {
	resp, err := c.Do(ctx, method, path, body, size)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, msg)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// FrameStatus is what the UI shows about the frame connection.
type FrameStatus struct {
	URL         string         `json:"url"`
	Online      bool           `json:"online"`
	LastSeen    time.Time      `json:"lastSeen,omitzero"`
	LastError   string         `json:"lastError,omitempty"`
	Info        *api.FrameInfo `json:"info,omitempty"`
	Syncing     bool           `json:"syncing"`
	PendingPush int            `json:"pendingPush"`
	LastSync    time.Time      `json:"lastSync,omitzero"`
}

// Syncer makes the frame's photo set match the server's: pushes missing or
// stale renditions (newest uploads first) and deletes photos removed here.
// It runs on a timer and immediately (debounced) when triggered.
type Syncer struct {
	client   *FrameClient
	store    *Store
	proc     *Processor
	events   *Broker
	log      *slog.Logger
	interval time.Duration
	autoSize bool
	trigger  chan struct{}

	mu     sync.Mutex
	status FrameStatus
}

func NewSyncer(cfg Config, store *Store, proc *Processor, events *Broker, log *slog.Logger) *Syncer {
	return &Syncer{
		client: &FrameClient{Base: cfg.FrameURL, Token: cfg.FrameToken, HTTP: &http.Client{Timeout: 60 * time.Second}},
		store:  store, proc: proc, events: events, log: log,
		interval: cfg.SyncInterval, autoSize: cfg.Width == 0 || cfg.Height == 0,
		trigger: make(chan struct{}, 1),
		status:  FrameStatus{URL: cfg.FrameURL},
	}
}

func (s *Syncer) Client() *FrameClient { return s.client }

// Trigger requests a sync soon. Safe to call often.
func (s *Syncer) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

func (s *Syncer) Status() FrameStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	return st
}

func (s *Syncer) update(fn func(*FrameStatus)) {
	s.mu.Lock()
	fn(&s.status)
	st := s.status
	s.mu.Unlock()
	s.events.Publish("frame", st)
}

func (s *Syncer) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	s.syncOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.trigger:
			// Debounce: let a burst of processed photos accumulate.
			select {
			case <-ctx.Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
			select {
			case <-s.trigger:
			default:
			}
		}
		s.syncOnce(ctx)
	}
}

func (s *Syncer) syncOnce(ctx context.Context) {
	var info api.FrameInfo
	if err := s.client.call(ctx, "GET", "/api/info", nil, -1, &info); err != nil {
		s.setOffline(err)
		return
	}
	wasOnline := s.Status().Online
	s.update(func(st *FrameStatus) {
		st.Online, st.LastSeen, st.LastError, st.Info = true, time.Now(), "", &info
	})
	if !wasOnline {
		s.log.Info("frame online", "url", s.client.Base, "version", info.Version,
			"display", fmt.Sprintf("%dx%d", info.Width, info.Height), "photos", info.PhotoCount)
	}
	if s.autoSize && info.Width > 0 && info.Height > 0 && s.store.SetDisplaySize(info.Width, info.Height) {
		s.log.Info("frame resolution changed; re-rendering all photos", "width", info.Width, "height", info.Height)
	}

	var manifest []api.ManifestEntry
	if err := s.client.call(ctx, "GET", "/api/photos", nil, -1, &manifest); err != nil {
		s.setOffline(err)
		return
	}
	onFrame := make(map[string]string, len(manifest))
	for _, m := range manifest {
		onFrame[m.ID] = m.Rev
	}

	rev := s.proc.Rev()
	photos := s.store.List() // newest first, so fresh uploads reach the frame first
	known := make(map[string]bool, len(photos))
	var push []Photo
	for _, p := range photos {
		known[p.ID] = true
		if p.Status != StatusReady || p.Rev == "" {
			continue
		}
		synced := onFrame[p.ID] == p.Rev
		if p.Synced != synced {
			s.store.Update(p.ID, func(x *Photo) { x.Synced = synced })
		}
		if p.Rev != rev {
			s.proc.Enqueue(p.ID) // stale rendition (resolution or renderer changed)
		}
		// Push current renditions; push a stale one only if the frame has nothing at all.
		if !synced && (p.Rev == rev || onFrame[p.ID] == "") {
			push = append(push, p)
		}
	}

	var del []string
	for id := range onFrame {
		if !known[id] {
			del = append(del, id)
		}
	}
	for _, id := range del {
		if err := s.client.call(ctx, "DELETE", "/api/photos/"+id, nil, -1, nil); err != nil {
			s.log.Warn("delete on frame failed", "id", id, "err", err)
		} else {
			s.log.Info("deleted from frame", "id", id)
		}
	}

	if len(push) > 0 {
		s.pushAll(ctx, push)
	}
	s.update(func(st *FrameStatus) { st.LastSync, st.PendingPush, st.Syncing = time.Now(), 0, false })
}

func (s *Syncer) pushAll(ctx context.Context, push []Photo) {
	start := time.Now()
	s.update(func(st *FrameStatus) { st.Syncing, st.PendingPush = true, len(push) })
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		next     atomic.Int64
		ok, fail atomic.Int64
		wg       sync.WaitGroup
	)
	for range min(pushConcurrency, len(push)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(push) || ctx.Err() != nil {
					return
				}
				p := push[i]
				if err := s.pushOne(ctx, p); err != nil {
					if ctx.Err() == nil {
						s.log.Warn("push to frame failed; will retry next sync", "id", p.ID, "name", p.Name, "err", err)
					}
					if fail.Add(1) >= 3 {
						cancel() // frame is probably unreachable; stop hammering it
					}
					continue
				}
				ok.Add(1)
				s.store.Update(p.ID, func(x *Photo) {
					if x.Rev == p.Rev {
						x.Synced = true
					}
				})
				s.update(func(st *FrameStatus) { st.PendingPush = max(st.PendingPush-1, 0) })
			}
		}()
	}
	wg.Wait()
	s.log.Info("sync push finished", "pushed", ok.Load(), "failed", fail.Load(),
		"dur", time.Since(start).Round(time.Millisecond))
}

func (s *Syncer) pushOne(ctx context.Context, p Photo) error {
	f, err := os.Open(s.store.DisplayPath(p.ID))
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	return s.client.call(ctx, "PUT", "/api/photos/"+p.ID+"?rev="+p.Rev, f, st.Size(), nil)
}

func (s *Syncer) setOffline(err error) {
	prev := s.Status()
	s.update(func(st *FrameStatus) { st.Online, st.LastError, st.Syncing = false, err.Error(), false })
	if prev.Online || prev.LastError == "" { // first failure, or just went down
		s.log.Warn("frame offline", "url", s.client.Base, "err", err)
	} else {
		s.log.Debug("frame unreachable", "url", s.client.Base, "err", err)
	}
}
