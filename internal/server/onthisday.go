package server

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the distroless image has no zoneinfo; time zones come from the browser

	"photoframe/internal/api"
)

// OnThisDay makes the frame regularly show photos taken on today's date in
// earlier years. Dates come from EXIF; photos without one never match.
type OnThisDay struct {
	Enabled       bool   `json:"enabled"`
	WindowDays    int    `json:"windowDays"`    // also match this many days before/after the date
	RepeatMinutes int    `json:"repeatMinutes"` // replay the group this often; 0 = once a day
	TimeZone      string `json:"timeZone"`      // IANA name sent by the browser; "" = server's
}

func DefaultOnThisDay() OnThisDay { return OnThisDay{WindowDays: 3, RepeatMinutes: 60} }

func (o *OnThisDay) Normalize() {
	o.WindowDays = min(max(o.WindowDays, 0), 15)
	o.RepeatMinutes = min(max(o.RepeatMinutes, 0), 24*60)
	if _, err := time.LoadLocation(o.TimeZone); err != nil {
		o.TimeZone = ""
	}
}

// Today returns today's date (midnight UTC) in the configured time zone.
func (o OnThisDay) Today(now time.Time) time.Time {
	loc := time.Local
	if o.TimeZone != "" {
		if l, err := time.LoadLocation(o.TimeZone); err == nil {
			loc = l
		}
	}
	y, m, d := now.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// yearsAgo reports whether taken falls within window days of today's date in
// an earlier year, and how many years back. Photos from the past few weeks
// (the same "year" as today) don't count.
func yearsAgo(taken, today time.Time, window int) (int, bool) {
	ty, tm, td := taken.Date()
	for y := today.Year() - 1; y <= today.Year()+1; y++ {
		if y <= ty {
			continue
		}
		anniv := time.Date(y, tm, td, 0, 0, 0, 0, time.UTC) // Feb 29 becomes Mar 1
		days := int(anniv.Sub(today).Hours() / 24)
		if days >= -window && days <= window {
			return y - ty, true
		}
	}
	return 0, false
}

// OnThisDayPhotos returns the ready photos matching today, oldest first.
func OnThisDayPhotos(photos []Photo, today time.Time, window int) []Photo {
	var out []Photo
	for _, p := range photos {
		if p.Status != StatusReady || p.TakenAt.IsZero() {
			continue
		}
		if _, ok := yearsAgo(p.TakenAt, today, window); ok {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b Photo) int {
		if c := a.TakenAt.Compare(b.TakenAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func (s *Store) OnThisDay() OnThisDay {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.otd
}

// SetOnThisDay saves o (already normalized).
func (s *Store) SetOnThisDay(o OnThisDay) {
	s.mu.Lock()
	s.otd = o
	s.mu.Unlock()
	s.markDirty()
}

// Featured is what the frame should currently treat as "On this day": only
// photos it already has, so the key changes (and the list is resent) as more
// of them arrive.
func (s *Store) Featured(now time.Time) api.Featured {
	o := s.OnThisDay()
	if !o.Enabled {
		return api.Featured{Key: "off"}
	}
	today := o.Today(now)
	f := api.Featured{RepeatMinutes: o.RepeatMinutes}
	h := fnv.New64a()
	fmt.Fprint(h, o.WindowDays, o.RepeatMinutes)
	for _, p := range OnThisDayPhotos(s.List(), today, o.WindowDays) {
		if p.Synced {
			f.IDs = append(f.IDs, p.ID)
			h.Write([]byte(p.ID))
		}
	}
	f.Key = fmt.Sprintf("%s-%x", today.Format("20060102"), h.Sum64())
	return f
}

// BackfillDates reads the date taken of photos uploaded before dates were
// recorded. It only reads the start of each original, where EXIF lives.
func BackfillDates(ctx context.Context, store *Store, log *slog.Logger) {
	start := time.Now()
	n, found := 0, 0
	buf := make([]byte, 256<<10)
	for _, p := range store.List() {
		if ctx.Err() != nil {
			return
		}
		if p.DateRead || p.Status == StatusProcessing {
			continue
		}
		f, err := os.Open(store.OriginalPath(p))
		if err != nil {
			continue
		}
		k, _ := io.ReadFull(f, buf)
		f.Close()
		taken := exifDateTaken(buf[:k])
		store.Update(p.ID, func(x *Photo) { x.TakenAt, x.DateRead = taken, true })
		n++
		if !taken.IsZero() {
			found++
		}
	}
	if n > 0 {
		log.Info("read photo dates", "photos", n, "withDate", found, "dur", time.Since(start).Round(time.Millisecond))
	}
}

type onThisDayPhoto struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	TakenAt  time.Time `json:"takenAt"`
	YearsAgo int       `json:"yearsAgo"`
	OnFrame  bool      `json:"onFrame"`
}

type onThisDayStatus struct {
	Settings OnThisDay        `json:"settings"`
	Today    string           `json:"today"`
	Photos   []onThisDayPhoto `json:"photos"`
	Dated    int              `json:"dated"` // photos with a known date taken
	Total    int              `json:"total"`
}

// onThisDayStatus describes today's matches for o. The windowDays and
// timeZone query parameters preview other values without saving them.
func (a *App) onThisDayStatus(o OnThisDay, r *http.Request) onThisDayStatus {
	q := r.URL.Query()
	if v, err := strconv.Atoi(q.Get("windowDays")); err == nil {
		o.WindowDays = v
	}
	if tz := q.Get("timeZone"); tz != "" {
		o.TimeZone = tz
	}
	o.Normalize()
	today := o.Today(time.Now())
	all := a.Store.List()
	st := onThisDayStatus{Settings: o, Today: today.Format("2006-01-02"), Photos: []onThisDayPhoto{}, Total: len(all)}
	for _, p := range all {
		if !p.TakenAt.IsZero() {
			st.Dated++
		}
	}
	for _, p := range OnThisDayPhotos(all, today, o.WindowDays) {
		ago, _ := yearsAgo(p.TakenAt, today, o.WindowDays)
		st.Photos = append(st.Photos, onThisDayPhoto{ID: p.ID, Name: p.Name, TakenAt: p.TakenAt, YearsAgo: ago, OnFrame: p.Synced})
	}
	return st
}

func (a *App) getOnThisDay(w http.ResponseWriter, r *http.Request) {
	api.WriteJSON(w, http.StatusOK, a.onThisDayStatus(a.Store.OnThisDay(), r))
}

func (a *App) putOnThisDay(w http.ResponseWriter, r *http.Request) {
	var o OnThisDay
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&o); err != nil {
		api.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	o.Normalize()
	a.Store.SetOnThisDay(o)
	a.Log.Info("on this day settings saved", "enabled", o.Enabled, "windowDays", o.WindowDays,
		"repeatMinutes", o.RepeatMinutes, "timeZone", o.TimeZone)
	if a.Sync != nil {
		a.Sync.Trigger()
	}
	api.WriteJSON(w, http.StatusOK, a.onThisDayStatus(o, r))
}
