package server

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// procVersion is part of every rendition's rev. Bump it when the rendering
// output changes so existing photos are re-rendered and re-pushed.
const procVersion = 1

// Processor turns originals into frame-ready renditions on a worker pool.
type Processor struct {
	store   *Store
	log     *slog.Logger
	quality int
	workers int
	onReady func(id string)
	q       *workQueue
}

func NewProcessor(store *Store, log *slog.Logger, cfg Config, onReady func(string)) *Processor {
	return &Processor{store: store, log: log, quality: cfg.JPEGQuality,
		workers: cfg.Workers, onReady: onReady, q: newWorkQueue()}
}

// Rev is the rendition tag for the current display size and background,
// e.g. "1920x1080v1", or "1920x1080v1c000000" with a black background.
func (p *Processor) Rev() string {
	w, h := p.store.DisplaySize()
	return revFor(w, h, p.store.Background())
}

func revFor(w, h int, bg Background) string {
	return fmt.Sprintf("%dx%dv%d%s", w, h, procVersion, bg.tag())
}

// revBase strips the background tag: "1920x1080v1c000000" → "1920x1080v1".
// Tags start with a letter; the part before has only digits after the "v".
func revBase(rev string) string {
	if i := strings.LastIndexByte(rev, 'v'); i >= 0 {
		if j := strings.IndexFunc(rev[i+1:], func(r rune) bool { return r < '0' || r > '9' }); j >= 0 {
			return rev[:i+1+j]
		}
	}
	return rev
}

func (p *Processor) Enqueue(id string) { p.q.push(id) }

// Pending returns queued plus in-progress jobs.
func (p *Processor) Pending() int { return p.q.len() }

func (p *Processor) Start(ctx context.Context) {
	for range p.workers {
		go func() {
			for {
				id, ok := p.q.pop(ctx)
				if !ok {
					return
				}
				p.process(id)
				p.q.done()
			}
		}()
	}
}

func (p *Processor) process(id string) {
	ph, ok := p.store.Get(id)
	if !ok {
		return
	}
	start := time.Now()
	fail := func(err error) {
		p.log.Error("processing failed", "id", id, "name", ph.Name, "err", err)
		p.store.Update(id, func(x *Photo) { x.Status, x.Error = StatusError, err.Error() })
	}
	defer func() {
		if r := recover(); r != nil { // malformed images can panic inside decoders
			fail(fmt.Errorf("decoder panic: %v", r))
		}
	}()

	w, h := p.store.DisplaySize()
	bg := p.store.Background()
	rev := revFor(w, h, bg)
	data, err := os.ReadFile(p.store.OriginalPath(ph))
	if err != nil {
		fail(err)
		return
	}
	orientation := exifOrientation(data)
	taken := exifDateTaken(data)
	img, format, err := image.Decode(bytes.NewReader(data))
	data = nil
	if err != nil {
		fail(fmt.Errorf("unsupported or corrupt image: %w", err))
		return
	}
	decoded := time.Now()
	srcW, srcH := img.Bounds().Dx(), img.Bounds().Dy()
	if orientation >= 5 {
		srcW, srcH = srcH, srcW
	}

	canvas, fitted := render(img, orientation, w, h, bg)
	img = nil
	if err := writeJPEG(p.store.DisplayPath(id), canvas, p.quality); err != nil {
		fail(err)
		return
	}
	// The thumbnail has no background, so a background-only change keeps it.
	thumbPath := p.store.ThumbPath(id)
	_, statErr := os.Stat(thumbPath)
	if statErr != nil || revBase(ph.Rev) != revBase(rev) {
		if err := writeJPEG(thumbPath, thumbnail(fitted, 480), 82); err != nil {
			fail(err)
			return
		}
	}
	p.store.Update(id, func(x *Photo) {
		x.Status, x.Error, x.Rev, x.Width, x.Height = StatusReady, "", rev, srcW, srcH
		x.Synced = false
		x.TakenAt, x.DateRead = taken, true
	})
	p.log.Info("photo processed", "id", id, "name", ph.Name, "format", format,
		"src", fmt.Sprintf("%dx%d", srcW, srcH), "orientation", orientation, "rev", rev,
		"decode", decoded.Sub(start).Round(time.Millisecond), "total", time.Since(start).Round(time.Millisecond))
	if p.onReady != nil {
		p.onReady(id)
	}
}

// workQueue is a FIFO of photo IDs with de-duplication.
type workQueue struct {
	mu      sync.Mutex
	items   []string
	pending map[string]bool
	active  int
	signal  chan struct{}
}

func newWorkQueue() *workQueue {
	return &workQueue{pending: map[string]bool{}, signal: make(chan struct{}, 1)}
}

func (q *workQueue) wake() {
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

func (q *workQueue) push(id string) {
	q.mu.Lock()
	if q.pending[id] {
		q.mu.Unlock()
		return
	}
	q.pending[id] = true
	q.items = append(q.items, id)
	q.mu.Unlock()
	q.wake()
}

func (q *workQueue) pop(ctx context.Context) (string, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			id := q.items[0]
			q.items = q.items[1:]
			delete(q.pending, id)
			q.active++
			more := len(q.items) > 0
			q.mu.Unlock()
			if more {
				q.wake() // chain-wake the next idle worker
			}
			return id, true
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", false
		case <-q.signal:
		}
	}
}

func (q *workQueue) done() {
	q.mu.Lock()
	q.active--
	q.mu.Unlock()
}

func (q *workQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) + q.active
}
