package server

import (
	"encoding/json"
	"fmt"
	"image/color"
	"image/jpeg"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"

	"photoframe/internal/api"
)

// Background controls how the screen area around a photo is filled when the
// photo's shape doesn't match the screen (e.g. portrait photos on a landscape
// screen). It's set from the web UI and stored in index.json.
type Background struct {
	Mode       string `json:"mode"`       // "blur" (blurred copy of the photo) or "color" (solid color)
	Color      string `json:"color"`      // "#rrggbb", used when Mode is "color"
	Brightness int    `json:"brightness"` // blur only: 0–100 % of the photo's brightness
	Blur       int    `json:"blur"`       // blur only: strength 1–10
}

const (
	defaultBrightness = 55
	defaultBlur       = 5
)

// DefaultBackground is the background used until one is saved in the UI.
// env is the legacy BACKGROUND variable ("blur" or "black").
func DefaultBackground(env string) Background {
	b := Background{Mode: "blur", Color: "#000000", Brightness: defaultBrightness, Blur: defaultBlur}
	if strings.EqualFold(strings.TrimSpace(env), "black") {
		b.Mode = "color"
	}
	return b
}

// Normalize clamps values into range and reports an error for invalid input.
func (b *Background) Normalize() error {
	b.Mode = strings.ToLower(strings.TrimSpace(b.Mode))
	if b.Mode != "blur" && b.Mode != "color" {
		return fmt.Errorf("mode must be \"blur\" or \"color\"")
	}
	b.Color = strings.ToLower(strings.TrimSpace(b.Color))
	if b.Color == "" {
		b.Color = "#000000"
	}
	if _, err := parseHexColor(b.Color); err != nil {
		return err
	}
	b.Brightness = min(max(b.Brightness, 0), 100)
	b.Blur = min(max(b.Blur, 1), 10)
	return nil
}

// tag is appended to the rendition rev so a background change re-renders and
// re-pushes every photo. The default background has no tag, which keeps revs
// from before this setting existed valid. Only [0-9a-z] (api.ValidRev).
func (b Background) tag() string {
	switch {
	case b.Mode == "color":
		return "c" + strings.TrimPrefix(b.Color, "#")
	case b.Brightness == defaultBrightness && b.Blur == defaultBlur:
		return ""
	default:
		return fmt.Sprintf("b%ds%d", b.Brightness, b.Blur)
	}
}

func (b Background) rgba() color.RGBA {
	c, _ := parseHexColor(b.Color)
	return c
}

func parseHexColor(s string) (color.RGBA, error) {
	if len(s) != 7 || s[0] != '#' {
		return color.RGBA{}, fmt.Errorf("color must look like #rrggbb")
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return color.RGBA{}, fmt.Errorf("color must look like #rrggbb")
	}
	return color.RGBA{R: byte(v >> 16), G: byte(v >> 8), B: byte(v), A: 255}, nil
}

// --- HTTP ---------------------------------------------------------------------

func (a *App) getBackground(w http.ResponseWriter, r *http.Request) {
	api.WriteJSON(w, http.StatusOK, a.Store.Background())
}

// putBackground saves the background and re-renders every photo with it. The
// syncer then replaces the frame's copies (until each is replaced, the frame
// keeps showing the old one).
func (a *App) putBackground(w http.ResponseWriter, r *http.Request) {
	var b Background
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := b.Normalize(); err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	n := 0
	if a.Store.SetBackground(b) {
		for _, p := range a.Store.List() {
			if p.Status == StatusReady {
				a.Proc.Enqueue(p.ID)
				n++
			}
		}
		a.Log.Info("photo background changed; re-rendering photos", "mode", b.Mode, "color", b.Color,
			"brightness", b.Brightness, "blur", b.Blur, "photos", n)
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"background": b, "rerendering": n})
}

// previewBackground renders a small sample of the frame's screen with the
// background given in the query string, using a photo's thumbnail so it's
// fast enough to update live while the user drags a slider. Without ?id=, it
// picks the photo whose shape differs most from the screen.
func (a *App) previewBackground(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	b := a.Store.Background()
	if v := q.Get("mode"); v != "" {
		b.Mode = v
	}
	if v := q.Get("color"); v != "" {
		b.Color = v
	}
	if v, err := strconv.Atoi(q.Get("brightness")); err == nil {
		b.Brightness = v
	}
	if v, err := strconv.Atoi(q.Get("blur")); err == nil {
		b.Blur = v
	}
	if err := b.Normalize(); err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	W, H := a.Store.DisplaySize()
	id := q.Get("id")
	if id == "" {
		id = a.samplePhoto(W, H)
	}
	if id == "" {
		api.WriteError(w, http.StatusNotFound, "no photos yet")
		return
	}
	f, err := os.Open(a.Store.ThumbPath(id))
	if err != nil {
		api.WriteError(w, http.StatusNotFound, "photo not ready")
		return
	}
	thumb, err := jpeg.Decode(f)
	f.Close()
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	pw := 640
	ph := max(1, pw*H/W)
	canvas, _ := render(thumb, 1, pw, ph, b)
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	jpeg.Encode(w, canvas, &jpeg.Options{Quality: 85})
}

// samplePhoto returns the ready photo whose aspect ratio is furthest from the
// screen's, i.e. the one that shows the most background.
func (a *App) samplePhoto(W, H int) string {
	screen := math.Log(float64(W) / float64(H))
	best, bestDiff := "", -1.0
	for _, p := range a.Store.List() {
		if p.Status != StatusReady || p.Width <= 0 || p.Height <= 0 {
			continue
		}
		if d := math.Abs(math.Log(float64(p.Width)/float64(p.Height)) - screen); d > bestDiff {
			best, bestDiff = p.ID, d
		}
	}
	return best
}
