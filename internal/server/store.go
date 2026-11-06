package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	StatusProcessing = "processing"
	StatusReady      = "ready"
	StatusError      = "error"
)

// Photo is the server's record of one uploaded photo. ID is the first 16
// bytes of the original file's SHA-256, so re-uploading the same file is a no-op.
type Photo struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Ext        string    `json:"ext"`
	Size       int64     `json:"size"`
	Width      int       `json:"width,omitempty"`
	Height     int       `json:"height,omitempty"`
	UploadedAt time.Time `json:"uploadedAt"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	Rev        string    `json:"rev,omitempty"` // rendition currently in display/
	Synced     bool      `json:"synced"`        // frame has this exact rendition
}

type indexFile struct {
	DisplayW int      `json:"displayW"`
	DisplayH int      `json:"displayH"`
	Photos   []*Photo `json:"photos"`
}

// Store keeps the photo index in memory and persists it to index.json
// (debounced). Files live under DATA_DIR/{originals,display,thumbs}.
type Store struct {
	dir    string
	log    *slog.Logger
	events *Broker

	mu       sync.RWMutex
	photos   map[string]*Photo
	displayW int
	displayH int
	dirty    chan struct{}
}

func OpenStore(dir string, log *slog.Logger, events *Broker) (*Store, error) {
	for _, sub := range []string{"originals", "display", "thumbs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	if tmps, _ := filepath.Glob(filepath.Join(dir, "tmp", "*")); len(tmps) > 0 {
		for _, t := range tmps {
			os.Remove(t)
		}
	}
	s := &Store{dir: dir, log: log, events: events, photos: map[string]*Photo{},
		displayW: 1920, displayH: 1080, dirty: make(chan struct{}, 1)}
	b, err := os.ReadFile(s.indexPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var idx indexFile
		if err := json.Unmarshal(b, &idx); err != nil {
			return nil, err
		}
		for _, p := range idx.Photos {
			s.photos[p.ID] = p
		}
		if idx.DisplayW > 0 && idx.DisplayH > 0 {
			s.displayW, s.displayH = idx.DisplayW, idx.DisplayH
		}
	}
	return s, nil
}

func (s *Store) indexPath() string            { return filepath.Join(s.dir, "index.json") }
func (s *Store) TmpDir() string               { return filepath.Join(s.dir, "tmp") }
func (s *Store) OriginalPath(p Photo) string  { return filepath.Join(s.dir, "originals", p.ID+p.Ext) }
func (s *Store) DisplayPath(id string) string { return filepath.Join(s.dir, "display", id+".jpg") }
func (s *Store) ThumbPath(id string) string   { return filepath.Join(s.dir, "thumbs", id+".jpg") }

func (s *Store) markDirty() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

// RunSaver persists the index at most once per second until ctx is done.
func (s *Store) RunSaver(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			s.save()
			return
		case <-s.dirty:
			s.save()
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

func (s *Store) save() {
	s.mu.RLock()
	idx := indexFile{DisplayW: s.displayW, DisplayH: s.displayH, Photos: make([]*Photo, 0, len(s.photos))}
	for _, p := range s.photos {
		cp := *p
		idx.Photos = append(idx.Photos, &cp)
	}
	s.mu.RUnlock()
	slices.SortFunc(idx.Photos, func(a, b *Photo) int { return a.UploadedAt.Compare(b.UploadedAt) })
	b, err := json.Marshal(idx)
	if err == nil {
		tmp := s.indexPath() + ".tmp"
		if err = os.WriteFile(tmp, b, 0o644); err == nil {
			err = os.Rename(tmp, s.indexPath())
		}
	}
	if err != nil {
		s.log.Error("saving index failed", "err", err)
	}
}

// Add inserts p; it returns false if the ID already exists.
func (s *Store) Add(p Photo) bool {
	s.mu.Lock()
	if _, ok := s.photos[p.ID]; ok {
		s.mu.Unlock()
		return false
	}
	s.photos[p.ID] = &p
	s.mu.Unlock()
	s.markDirty()
	s.events.Publish("photo", p)
	return true
}

func (s *Store) Get(id string) (Photo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.photos[id]; ok {
		return *p, true
	}
	return Photo{}, false
}

// List returns all photos, newest upload first.
func (s *Store) List() []Photo {
	s.mu.RLock()
	out := make([]Photo, 0, len(s.photos))
	for _, p := range s.photos {
		out = append(out, *p)
	}
	s.mu.RUnlock()
	slices.SortFunc(out, func(a, b Photo) int {
		if c := b.UploadedAt.Compare(a.UploadedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// Update applies fn to the photo and broadcasts the change if anything changed.
func (s *Store) Update(id string, fn func(*Photo)) (Photo, bool) {
	s.mu.Lock()
	p, ok := s.photos[id]
	if !ok {
		s.mu.Unlock()
		return Photo{}, false
	}
	before := *p
	fn(p)
	after := *p
	s.mu.Unlock()
	if after != before {
		s.markDirty()
		s.events.Publish("photo", after)
	}
	return after, true
}

// Delete removes the photo and all its files.
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	p, ok := s.photos[id]
	delete(s.photos, id)
	s.mu.Unlock()
	if !ok {
		return false
	}
	os.Remove(s.OriginalPath(*p))
	os.Remove(s.DisplayPath(id))
	os.Remove(s.ThumbPath(id))
	s.markDirty()
	s.events.Publish("deleted", map[string]string{"id": id})
	return true
}

func (s *Store) DisplaySize() (int, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.displayW, s.displayH
}

// SetDisplaySize reports whether the size changed.
func (s *Store) SetDisplaySize(w, h int) bool {
	s.mu.Lock()
	changed := w != s.displayW || h != s.displayH
	s.displayW, s.displayH = w, h
	s.mu.Unlock()
	if changed {
		s.markDirty()
	}
	return changed
}
