package frame

// Display is something frames can be shown on.
type Display interface {
	Size() (w, h int)
	Format() PixelFormat
	Show(*Frame) error
	Name() string
	Close() error
}

// Headless is a no-op display used for local development on non-Linux
// machines, or as a fallback when the framebuffer can't be opened (so the API
// and logs stay reachable for troubleshooting).
type Headless struct{ w, h int }

func NewHeadless(w, h int) *Headless { return &Headless{w: w, h: h} }

func (d *Headless) Size() (int, int)    { return d.w, d.h }
func (d *Headless) Format() PixelFormat { return RGBX32 }
func (d *Headless) Name() string        { return "headless" }
func (d *Headless) Close() error        { return nil }
func (d *Headless) Show(*Frame) error   { return nil }
