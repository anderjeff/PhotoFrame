// Package api holds the wire types and helpers shared by the frame (Pi) and
// the server (NAS). Anything that crosses the network between them lives here.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"regexp"
)

// TokenHeader carries the shared secret on every server→frame request.
const TokenHeader = "X-PhotoFrame-Token"

// Settings controls slideshow behaviour. The frame owns and persists them;
// the server UI reads/writes them through the frame API.
type Settings struct {
	IntervalSeconds int    `json:"intervalSeconds"`
	Shuffle         bool   `json:"shuffle"`
	Transition      string `json:"transition"` // "fade" or "none"
	TransitionMs    int    `json:"transitionMs"`
	ShowNewFirst    bool   `json:"showNewFirst"` // newly received photos jump the queue
}

func DefaultSettings() Settings {
	return Settings{IntervalSeconds: 30, Shuffle: true, Transition: "fade", TransitionMs: 800, ShowNewFirst: true}
}

// Normalize clamps values into sane ranges.
func (s *Settings) Normalize() {
	s.IntervalSeconds = min(max(s.IntervalSeconds, 3), 86400)
	if s.Transition != "none" {
		s.Transition = "fade"
	}
	s.TransitionMs = min(max(s.TransitionMs, 0), 5000)
}

// FrameInfo is the frame's self-reported status (GET /api/info).
type FrameInfo struct {
	Version        string   `json:"version"`
	Hostname       string   `json:"hostname"`
	Addresses      []string `json:"addresses"`
	Display        string   `json:"display"`
	Width          int      `json:"width"`
	Height         int      `json:"height"`
	PhotoCount     int      `json:"photoCount"`
	Current        string   `json:"current"`
	Paused         bool     `json:"paused"`
	UptimeSeconds  int64    `json:"uptimeSeconds"`
	CPUTempC       float64  `json:"cpuTempC"`
	DiskFreeBytes  uint64   `json:"diskFreeBytes"`
	DiskTotalBytes uint64   `json:"diskTotalBytes"`
	MemAvailBytes  uint64   `json:"memAvailBytes"`
	Settings       Settings `json:"settings"`
}

// ManifestEntry describes one server-managed photo stored on the frame.
// Rev identifies the rendition (resolution + processor version) so the
// server can detect stale copies and re-push.
type ManifestEntry struct {
	ID   string `json:"id"`
	Rev  string `json:"rev"`
	Size int64  `json:"size"`
}

var (
	idRe  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	revRe = regexp.MustCompile(`^[0-9a-z]{1,32}$`)
)

// ValidID reports whether s is a photo ID (first 16 bytes of the original's SHA-256, hex).
func ValidID(s string) bool { return idRe.MatchString(s) }

// ValidRev reports whether s is a rendition tag such as "1920x1080v1".
func ValidRev(s string) bool { return revRe.MatchString(s) }

// RequireToken rejects requests without the shared token. An empty token disables the check.
func RequireToken(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(TokenHeader)), want) != 1 {
			WriteError(w, http.StatusUnauthorized, "missing or invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}
