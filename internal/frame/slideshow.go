package frame

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	_ "image/png"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

	"photoframe/internal/api"
)

type evKind int

const (
	evNext evKind = iota
	evPrev
	evPause
	evResume
	evAdded
	evRemoved
	evReplaced
	evSettings
	evFeatured
)

type event struct {
	kind evKind
	keys []string
	dur  time.Duration
}

type prefetch struct {
	key   string
	frame *Frame
	err   error
	done  chan struct{}
}

// Slideshow drives the display. All playlist state is owned by the Run
// goroutine; other goroutines talk to it through the events channel, and only
// read the small mu-protected status block.
type Slideshow struct {
	log          *slog.Logger
	store        *Store
	disp         Display
	settingsPath string
	emptyLines   func() []string

	events chan event

	// Owned by Run.
	fresh      []string // newly received photos, shown before the regular queue
	featured   []string // "On this day" photos, shown as a group every featRepeat
	featQ      []string // the rest of the group currently being shown
	featNext   time.Time
	featRepeat time.Duration // 0 = once per Featured
	queue      []string
	history    []string
	bad        map[string]bool
	pre        *prefetch
	scratch    *Frame
	emptyShown bool

	mu       sync.RWMutex
	settings api.Settings
	current  string
	paused   bool
	curFrame *Frame
	shownAt  time.Time
	featKey  string
}

// NewSlideshow loads settings from settingsPath (defaults if missing).
// emptyLines supplies the text shown when there are no photos.
func NewSlideshow(log *slog.Logger, store *Store, disp Display, settingsPath string, emptyLines func() []string) *Slideshow {
	s := &Slideshow{
		log: log, store: store, disp: disp, settingsPath: settingsPath, emptyLines: emptyLines,
		events: make(chan event, 1024), bad: map[string]bool{}, settings: api.DefaultSettings(),
	}
	if b, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(b, &s.settings); err != nil {
			log.Warn("ignoring corrupt settings file", "path", settingsPath, "err", err)
			s.settings = api.DefaultSettings()
		}
	}
	s.settings.Normalize()
	return s
}

// StoreChanged is wired to Store.SetOnChange.
func (s *Slideshow) StoreChanged(added, removed []string) {
	if len(removed) > 0 {
		s.events <- event{kind: evRemoved, keys: removed}
	}
	if len(added) > 0 {
		s.events <- event{kind: evAdded, keys: added}
	}
}

// StoreReplaced is wired to Store.SetOnReplace.
func (s *Slideshow) StoreReplaced(key string) {
	s.events <- event{kind: evReplaced, keys: []string{key}}
}

// SetFeatured replaces the "On this day" group. The group starts with the
// next photo and repeats every RepeatMinutes.
func (s *Slideshow) SetFeatured(f api.Featured) {
	ids := slices.DeleteFunc(slices.Clone(f.IDs), func(id string) bool { return !api.ValidID(id) })
	s.events <- event{kind: evFeatured, keys: ids, dur: time.Duration(max(f.RepeatMinutes, 0)) * time.Minute}
	s.mu.Lock()
	s.featKey = f.Key
	s.mu.Unlock()
}

// FeaturedKey is the key of the last SetFeatured.
func (s *Slideshow) FeaturedKey() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.featKey
}

// Control handles "next", "prev", "pause", "resume".
func (s *Slideshow) Control(action string) error {
	k, ok := map[string]evKind{"next": evNext, "prev": evPrev, "pause": evPause, "resume": evResume}[action]
	if !ok {
		return fmt.Errorf("unknown action %q", action)
	}
	s.events <- event{kind: k}
	return nil
}

func (s *Slideshow) Settings() api.Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings
}

func (s *Slideshow) SetSettings(in api.Settings) (api.Settings, error) {
	in.Normalize()
	b, _ := json.MarshalIndent(in, "", "  ")
	tmp := s.settingsPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return in, err
	}
	if err := os.Rename(tmp, s.settingsPath); err != nil {
		return in, err
	}
	s.mu.Lock()
	s.settings = in
	s.mu.Unlock()
	s.events <- event{kind: evSettings}
	s.log.Info("settings updated", "interval", in.IntervalSeconds, "shuffle", in.Shuffle, "transition", in.Transition)
	return in, nil
}

// Status returns the current photo key and paused flag.
func (s *Slideshow) Status() (current string, paused bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current, s.paused
}

// Screenshot returns what is currently on screen.
func (s *Slideshow) Screenshot() *image.RGBA {
	s.mu.RLock()
	fr := s.curFrame
	s.mu.RUnlock()
	if fr == nil {
		return nil
	}
	return fr.ToImage()
}

// Run drives the slideshow until done is closed.
func (s *Slideshow) Run(done <-chan struct{}) {
	s.advance(1)
	timer := time.NewTimer(s.interval())
	defer timer.Stop()
	resetIn := func(d time.Duration) { timer.Reset(d) }
	reset := func() { resetIn(s.interval()) }

	for {
		select {
		case <-done:
			return
		case <-timer.C:
			if _, paused := s.Status(); !paused {
				s.advance(1)
			}
			reset()
		case ev := <-s.events:
			s.handle(ev, reset, resetIn)
		}
	}
}

func (s *Slideshow) interval() time.Duration {
	return time.Duration(s.Settings().IntervalSeconds) * time.Second
}

func (s *Slideshow) handle(ev event, reset func(), resetIn func(time.Duration)) {
	switch ev.kind {
	case evNext:
		s.advance(1)
		reset()
	case evPrev:
		s.advance(-1)
		reset()
	case evPause:
		s.setPaused(true)
	case evResume:
		s.setPaused(false)
		reset()
	case evSettings:
		s.queue = nil // rebuild with new shuffle setting
		s.ensurePrefetch()
		reset()
	case evAdded:
		for _, k := range ev.keys {
			delete(s.bad, k)
		}
		if s.Settings().ShowNewFirst {
			s.fresh = append(s.fresh, ev.keys...)
		} else {
			for _, k := range ev.keys {
				i := len(s.queue)
				if s.Settings().Shuffle {
					i = rand.IntN(len(s.queue) + 1)
				}
				s.queue = slices.Insert(s.queue, i, k)
			}
		}
		cur, paused := s.Status()
		switch {
		case cur == "":
			s.advance(1)
			reset()
		case s.Settings().ShowNewFirst && !paused && time.Since(s.shownAtTime()) > 5*time.Second:
			// Show the first new arrival promptly; the rest follow at the normal interval.
			resetIn(time.Second)
		}
		s.ensurePrefetch()
	case evFeatured:
		s.featured, s.featRepeat, s.featQ, s.featNext = ev.keys, ev.dur, nil, time.Time{}
		s.log.Info("on this day updated", "photos", len(ev.keys), "repeat", ev.dur)
		if p := s.pre; p != nil {
			<-p.done // drop the prefetched regular photo so the group starts next
			s.pre = nil
		}
		s.ensurePrefetch()
	case evReplaced:
		// The photo stays in rotation; only decoded copies of the old file are stale.
		k := ev.keys[0]
		delete(s.bad, k)
		if p := s.pre; p != nil && p.key == k {
			<-p.done // keep the one-decode-at-a-time rule; it's at most one photo's work
			s.pre = nil
		}
		if cur, _ := s.Status(); cur == k {
			// Redraw it now so a change (e.g. a new background) shows up right away.
			if fr, err := s.load(k); err != nil {
				s.log.Warn("reloading replaced photo failed", "key", k, "err", err)
			} else {
				s.transition(fr)
				s.mu.Lock()
				s.curFrame = fr
				s.mu.Unlock()
				s.log.Debug("reloaded replaced photo", "key", k)
			}
		}
		s.ensurePrefetch()
	case evRemoved:
		gone := make(map[string]bool, len(ev.keys))
		for _, k := range ev.keys {
			gone[k] = true
		}
		notGone := func(k string) bool { return gone[k] }
		s.fresh = slices.DeleteFunc(s.fresh, notGone)
		s.featured = slices.DeleteFunc(s.featured, notGone)
		s.featQ = slices.DeleteFunc(s.featQ, notGone)
		s.queue = slices.DeleteFunc(s.queue, notGone)
		s.history = slices.DeleteFunc(s.history, notGone)
		if s.pre != nil && gone[s.pre.key] {
			s.pre = nil
		}
		if cur, _ := s.Status(); gone[cur] {
			s.advance(1)
			reset()
		} else {
			s.ensurePrefetch()
		}
	}
}

func (s *Slideshow) setPaused(p bool) {
	s.mu.Lock()
	s.paused = p
	s.mu.Unlock()
	s.log.Info("slideshow", "paused", p)
}

func (s *Slideshow) shownAtTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.shownAt
}

func (s *Slideshow) valid(k string) bool {
	_, ok := s.store.Get(k)
	return ok && !s.bad[k]
}

// refill rebuilds the regular queue from every photo on disk.
func (s *Slideshow) refill(cur string) {
	clear(s.bad) // give previously failing files another chance each cycle
	keys := s.store.Keys()
	if len(keys) > 1 {
		keys = slices.DeleteFunc(keys, func(k string) bool { return k == cur })
	}
	if s.Settings().Shuffle {
		rand.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	}
	s.queue = keys
}

func (s *Slideshow) nextKey(cur string, pop bool) string {
	if len(s.featQ) == 0 && len(s.featured) > 0 && !time.Now().Before(s.featNext) {
		s.featQ = slices.Clone(s.featured)
		s.featNext = time.Now().Add(s.featRepeat)
		if s.featRepeat <= 0 {
			s.featNext = time.Now().AddDate(100, 0, 0)
		}
		s.log.Info("showing On this day photos", "photos", len(s.featQ))
	}
	for _, q := range []*[]string{&s.fresh, &s.featQ, &s.queue} {
		for len(*q) > 0 {
			k := (*q)[0]
			if s.valid(k) && k != cur {
				if pop {
					*q = (*q)[1:]
				}
				return k
			}
			*q = (*q)[1:]
		}
	}
	s.refill(cur)
	for len(s.queue) > 0 {
		k := s.queue[0]
		if pop {
			s.queue = s.queue[1:]
		}
		if s.valid(k) {
			return k
		}
		if !pop {
			s.queue = s.queue[1:]
		}
	}
	return ""
}

func (s *Slideshow) advance(dir int) {
	for range 5 {
		cur, _ := s.Status()
		var key string
		if dir < 0 {
			if len(s.history) == 0 {
				return
			}
			key = s.history[len(s.history)-1]
			s.history = s.history[:len(s.history)-1]
			if cur != "" {
				s.fresh = append([]string{cur}, s.fresh...)
			}
		} else {
			key = s.nextKey(cur, true)
		}
		if key == "" {
			if cur != "" {
				if _, ok := s.store.Get(cur); ok {
					return // the only photo is already on screen
				}
			}
			s.showEmpty()
			return
		}
		if key == cur {
			return
		}
		fr, err := s.prepare(key)
		if err != nil {
			s.log.Warn("skipping unreadable photo", "key", key, "err", err)
			s.bad[key] = true
			continue
		}
		s.transition(fr)
		if dir > 0 && cur != "" {
			s.history = append(s.history, cur)
			if len(s.history) > 100 {
				s.history = s.history[1:]
			}
		}
		s.mu.Lock()
		s.current, s.curFrame, s.shownAt = key, fr, time.Now()
		s.mu.Unlock()
		s.emptyShown = false
		s.log.Debug("showing photo", "key", key)
		s.ensurePrefetch()
		return
	}
}

// ensurePrefetch decodes the upcoming photo in the background so the next
// advance is instant. Only one prefetch runs at a time, which keeps a burst of
// incoming photos from piling decode work onto the Pi.
func (s *Slideshow) ensurePrefetch() {
	if s.pre != nil {
		select {
		case <-s.pre.done:
		default:
			return // one already in flight
		}
	}
	cur, _ := s.Status()
	k := s.nextKey(cur, false)
	if k == "" || k == cur || (s.pre != nil && s.pre.key == k && s.pre.err == nil) {
		return
	}
	p := &prefetch{key: k, done: make(chan struct{})}
	s.pre = p
	go func() {
		defer close(p.done)
		p.frame, p.err = s.load(k)
	}()
}

func (s *Slideshow) prepare(key string) (*Frame, error) {
	if p := s.pre; p != nil && p.key == key {
		<-p.done
		s.pre = nil
		if p.err == nil {
			return p.frame, nil
		}
	}
	return s.load(key)
}

// load decodes a photo into a display-ready frame. Server-rendered photos are
// already exactly screen-sized; anything else is letterboxed here.
func (s *Slideshow) load(key string) (*Frame, error) {
	p, ok := s.store.Get(key)
	if !ok {
		return nil, errors.New("photo no longer exists")
	}
	start := time.Now()
	data, err := os.ReadFile(p.Path)
	if err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	decoded := time.Now()
	w, h := s.disp.Size()
	scaled := false
	if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
		img = fitImage(img, w, h)
		scaled = true
	}
	fr := NewFrame(w, h, s.disp.Format())
	fr.FromImage(img)
	s.log.Debug("photo prepared", "key", key, "bytes", len(data), "scaled", scaled,
		"decode", decoded.Sub(start).Round(time.Millisecond), "total", time.Since(start).Round(time.Millisecond))
	return fr, nil
}

// transition shows fr, cross-fading from the current frame if enabled. It
// renders as many blend steps as the hardware manages within the duration.
func (s *Slideshow) transition(to *Frame) {
	s.mu.RLock()
	from := s.curFrame
	s.mu.RUnlock()
	set := s.Settings()
	d := time.Duration(set.TransitionMs) * time.Millisecond
	if from != nil && set.Transition == "fade" && d > 0 && from.Fmt == to.Fmt && from.W == to.W && from.H == to.H {
		if s.scratch == nil || s.scratch.W != to.W || s.scratch.H != to.H || s.scratch.Fmt != to.Fmt {
			s.scratch = NewFrame(to.W, to.H, to.Fmt)
		}
		start := time.Now()
		steps := 0
		for {
			t := float64(time.Since(start)) / float64(d)
			if t >= 1 {
				break
			}
			t = t * t * (3 - 2*t) // smoothstep
			stepStart := time.Now()
			Blend(s.scratch, from, to, int(t*256))
			_ = s.disp.Show(s.scratch)
			steps++
			// Cap at ~60 fps; on a Pi 3 the blend itself is usually the limit.
			if rest := 16*time.Millisecond - time.Since(stepStart); rest > 0 {
				time.Sleep(rest)
			}
		}
		s.log.Debug("fade", "steps", steps, "dur", time.Since(start).Round(time.Millisecond))
	}
	if err := s.disp.Show(to); err != nil {
		s.log.Error("display failed", "err", err)
	}
}

func (s *Slideshow) showEmpty() {
	if s.emptyShown {
		return
	}
	w, h := s.disp.Size()
	fr := NewFrame(w, h, s.disp.Format())
	fr.FromImage(renderMessage(w, h, s.emptyLines()))
	s.transition(fr)
	s.mu.Lock()
	s.current, s.curFrame, s.shownAt = "", fr, time.Now()
	s.mu.Unlock()
	s.emptyShown = true
	s.log.Info("no photos to show; displaying placeholder")
}

// fitImage letterboxes img into a w×h black canvas (fallback for files not
// produced by the server, e.g. copied onto the SD card by hand).
func fitImage(img image.Image, w, h int) *image.RGBA {
	b := img.Bounds()
	scale := min(float64(w)/float64(b.Dx()), float64(h)/float64(b.Dy()))
	fw, fh := max(1, int(float64(b.Dx())*scale+0.5)), max(1, int(float64(b.Dy())*scale+0.5))
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.Black, image.Point{}, draw.Src)
	x0, y0 := (w-fw)/2, (h-fh)/2
	xdraw.ApproxBiLinear.Scale(dst, image.Rect(x0, y0, x0+fw, y0+fh), img, b, xdraw.Src, nil)
	return dst
}

// renderMessage draws centered lines of text using the built-in bitmap font,
// scaled up with nearest-neighbour so it stays crisp.
func renderMessage(w, h int, lines []string) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{18, 18, 22, 255}), image.Point{}, draw.Src)
	face := basicfont.Face7x13
	scale := max(2, h/300)
	lineH := 13*scale + scale*6
	y := (h - len(lines)*lineH) / 2
	for i, line := range lines {
		if line == "" {
			y += lineH
			continue
		}
		clr := color.RGBA{150, 150, 160, 255}
		if i == 0 {
			clr = color.RGBA{235, 235, 240, 255}
		}
		small := image.NewRGBA(image.Rect(0, 0, len(line)*7, 13))
		d := font.Drawer{Dst: small, Src: image.NewUniform(clr), Face: face, Dot: fixed.P(0, 11)}
		d.DrawString(line)
		sw := small.Bounds().Dx() * scale
		x := (w - sw) / 2
		xdraw.NearestNeighbor.Scale(img, image.Rect(x, y, x+sw, y+13*scale), small, small.Bounds(), xdraw.Over, nil)
		y += lineH
	}
	return img
}

// SettingsPath returns the default settings file location inside dataDir.
func SettingsPath(dataDir string) string { return filepath.Join(dataDir, "settings.json") }
