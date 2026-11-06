package frame

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"photoframe/internal/api"
)

// Photo is one image file on the SD card.
//
// Server-managed photos are named "<id>_<rev>.jpg" and keyed by id. Any other
// .jpg/.jpeg/.png dropped into the directory by hand is also shown, keyed by
// filename, and is never touched by sync.
type Photo struct {
	Key     string
	ID, Rev string // empty for manually added files
	Path    string
	Size    int64
	Managed bool
}

var ErrNotJPEG = errors.New("body is not a JPEG")

// Store tracks the photo directory. Writes are atomic (temp file, fsync,
// rename) so a power cut never leaves a half-written photo behind.
type Store struct {
	dir string
	log *slog.Logger

	mu       sync.RWMutex
	photos   map[string]Photo
	onChange func(added, removed []string)
}

func OpenStore(dir string, log *slog.Logger) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, log: log, photos: map[string]Photo{}}
	if tmps, _ := filepath.Glob(filepath.Join(dir, ".upload-*.tmp")); len(tmps) > 0 {
		for _, t := range tmps {
			os.Remove(t)
		}
		log.Info("removed leftover temp files", "count", len(tmps))
	}
	if err := s.Scan(); err != nil {
		return nil, err
	}
	return s, nil
}

// SetOnChange registers the callback invoked (outside locks) when photos are added or removed.
func (s *Store) SetOnChange(fn func(added, removed []string)) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

func (s *Store) notify(added, removed []string) {
	s.mu.RLock()
	fn := s.onChange
	s.mu.RUnlock()
	if fn != nil && (len(added) > 0 || len(removed) > 0) {
		fn(added, removed)
	}
}

func parseName(name string) (Photo, bool) {
	if strings.HasPrefix(name, ".") {
		return Photo{}, false
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
		return Photo{}, false
	}
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if id, rev, ok := strings.Cut(base, "_"); ok && ext == ".jpg" && api.ValidID(id) && api.ValidRev(rev) {
		return Photo{Key: id, ID: id, Rev: rev, Managed: true}, true
	}
	return Photo{Key: name}, true
}

// Scan re-reads the directory, picking up files added or removed by hand.
func (s *Store) Scan() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	found := make(map[string]Photo, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		p, ok := parseName(e.Name())
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		p.Path = filepath.Join(s.dir, e.Name())
		p.Size = info.Size()
		if prev, dup := found[p.Key]; dup && p.Managed {
			// Two renditions of the same id (interrupted replace). Keep the newer.
			prevInfo, _ := os.Stat(prev.Path)
			if prevInfo != nil && prevInfo.ModTime().After(info.ModTime()) {
				os.Remove(p.Path)
				continue
			}
			os.Remove(prev.Path)
		}
		found[p.Key] = p
	}

	s.mu.Lock()
	var added, removed []string
	for k := range found {
		if _, ok := s.photos[k]; !ok {
			added = append(added, k)
		}
	}
	for k := range s.photos {
		if _, ok := found[k]; !ok {
			removed = append(removed, k)
		}
	}
	s.photos = found
	s.mu.Unlock()
	slices.Sort(added)
	if len(added)+len(removed) > 0 {
		s.log.Info("photo directory scanned", "total", len(found), "added", len(added), "removed", len(removed))
	}
	s.notify(added, removed)
	return nil
}

// Put stores a server-managed photo, replacing any other rendition of the same id.
func (s *Store) Put(id, rev string, r io.Reader, maxBytes int64) error {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(r, hdr); err != nil || hdr[0] != 0xFF || hdr[1] != 0xD8 {
		return ErrNotJPEG
	}
	tmp, err := os.CreateTemp(s.dir, ".upload-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after successful rename
	n, err := tmp.Write(hdr)
	if err == nil {
		var m int64
		m, err = io.Copy(tmp, io.LimitReader(r, maxBytes))
		n += int(m)
		if err == nil && int64(n) > maxBytes {
			err = fmt.Errorf("photo exceeds %d bytes", maxBytes)
		}
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	final := filepath.Join(s.dir, id+"_"+rev+".jpg")
	if err := os.Rename(tmp.Name(), final); err != nil {
		return err
	}

	s.mu.Lock()
	old, existed := s.photos[id]
	s.photos[id] = Photo{Key: id, ID: id, Rev: rev, Path: final, Size: int64(n), Managed: true}
	s.mu.Unlock()
	if existed && old.Path != final {
		os.Remove(old.Path)
	}
	if !existed {
		s.notify([]string{id}, nil)
	}
	return nil
}

// Delete removes a photo by key. It reports whether the photo existed.
func (s *Store) Delete(key string) (bool, error) {
	s.mu.Lock()
	p, ok := s.photos[key]
	delete(s.photos, key)
	s.mu.Unlock()
	if !ok {
		return false, nil
	}
	err := os.Remove(p.Path)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	s.notify(nil, []string{key})
	return true, err
}

func (s *Store) Get(key string) (Photo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.photos[key]
	return p, ok
}

func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.photos)
}

// Keys returns all photo keys, sorted.
func (s *Store) Keys() []string {
	s.mu.RLock()
	keys := make([]string, 0, len(s.photos))
	for k := range s.photos {
		keys = append(keys, k)
	}
	s.mu.RUnlock()
	slices.Sort(keys)
	return keys
}

// Manifest lists server-managed photos only.
func (s *Store) Manifest() []api.ManifestEntry {
	s.mu.RLock()
	out := make([]api.ManifestEntry, 0, len(s.photos))
	for _, p := range s.photos {
		if p.Managed {
			out = append(out, api.ManifestEntry{ID: p.ID, Rev: p.Rev, Size: p.Size})
		}
	}
	s.mu.RUnlock()
	slices.SortFunc(out, func(a, b api.ManifestEntry) int { return strings.Compare(a.ID, b.ID) })
	return out
}
