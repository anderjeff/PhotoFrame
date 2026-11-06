//go:build linux

package frame

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	fbioGetVScreenInfo = 0x4600
	fbioGetFScreenInfo = 0x4602
	kdSetMode          = 0x4B3A
	kdText             = 0x00
	kdGraphics         = 0x01
)

// Mirrors struct fb_bitfield / fb_var_screeninfo / fb_fix_screeninfo from
// <linux/fb.h>. Go's alignment matches C's for these on arm and arm64.
type fbBitfield struct{ Offset, Length, MsbRight uint32 }

type fbVarScreenInfo struct {
	Xres, Yres, XresVirtual, YresVirtual, Xoffset, Yoffset, BitsPerPixel, Grayscale uint32
	Red, Green, Blue, Transp                                                        fbBitfield
	Nonstd, Activate, Height, Width, AccelFlags, Pixclock                           uint32
	LeftMargin, RightMargin, UpperMargin, LowerMargin, HsyncLen, VsyncLen           uint32
	Sync, Vmode, Rotate, Colorspace                                                 uint32
	Reserved                                                                        [4]uint32
}

type fbFixScreenInfo struct {
	ID                            [16]byte
	SmemStart                     uintptr
	SmemLen                       uint32
	Type, TypeAux, Visual         uint32
	XPanStep, YPanStep, YWrapStep uint16
	LineLength                    uint32
	MmioStart                     uintptr
	MmioLen                       uint32
	Accel                         uint32
	Capabilities                  uint16
	Reserved                      [2]uint16
}

type fbDisplay struct {
	f          *os.File
	mem        []byte
	w, h       int
	lineLen    int
	xoff, yoff int
	format     PixelFormat
	tty        *os.File
	name       string
}

func ioctl(fd uintptr, req uintptr, arg uintptr) error {
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

// OpenFramebuffer maps dev (e.g. /dev/fb0). If tty is non-empty (e.g.
// /dev/tty1) it is switched to graphics mode so the text console doesn't draw
// a cursor or login prompt over the photos.
func OpenFramebuffer(dev, tty string) (Display, error) {
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	var v fbVarScreenInfo
	var fx fbFixScreenInfo
	if err := ioctl(f.Fd(), fbioGetVScreenInfo, uintptr(unsafe.Pointer(&v))); err != nil {
		f.Close()
		return nil, fmt.Errorf("FBIOGET_VSCREENINFO: %w", err)
	}
	if err := ioctl(f.Fd(), fbioGetFScreenInfo, uintptr(unsafe.Pointer(&fx))); err != nil {
		f.Close()
		return nil, fmt.Errorf("FBIOGET_FSCREENINFO: %w", err)
	}

	var pf PixelFormat
	switch v.BitsPerPixel {
	case 32:
		pf = PixelFormat{BPP: 32, ROff: int(v.Red.Offset / 8), GOff: int(v.Green.Offset / 8), BOff: int(v.Blue.Offset / 8)}
	case 16:
		pf = PixelFormat{BPP: 16}
	default:
		f.Close()
		return nil, fmt.Errorf("unsupported framebuffer depth %d bpp (need 16 or 32)", v.BitsPerPixel)
	}

	mem, err := unix.Mmap(int(f.Fd()), 0, int(fx.SmemLen), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmap: %w", err)
	}
	d := &fbDisplay{
		f: f, mem: mem, w: int(v.Xres), h: int(v.Yres), lineLen: int(fx.LineLength),
		xoff: int(v.Xoffset), yoff: int(v.Yoffset), format: pf,
		name: fmt.Sprintf("%s %dx%d %dbpp", dev, v.Xres, v.Yres, v.BitsPerPixel),
	}
	if need := (d.yoff+d.h-1)*d.lineLen + (d.xoff+d.w)*pf.bytesPP(); need > len(mem) {
		d.Close()
		return nil, fmt.Errorf("framebuffer memory too small (%d < %d)", len(mem), need)
	}
	if tty != "" {
		if t, err := os.OpenFile(tty, os.O_RDWR, 0); err == nil {
			if ioctl(t.Fd(), kdSetMode, kdGraphics) == nil {
				d.tty = t
			} else {
				t.Close()
			}
		}
	}
	return d, nil
}

func (d *fbDisplay) Size() (int, int)    { return d.w, d.h }
func (d *fbDisplay) Format() PixelFormat { return d.format }
func (d *fbDisplay) Name() string        { return d.name }

func (d *fbDisplay) Show(fr *Frame) error {
	if fr.W != d.w || fr.H != d.h || fr.Fmt != d.format {
		return fmt.Errorf("frame %dx%d does not match display %dx%d", fr.W, fr.H, d.w, d.h)
	}
	bpp := d.format.bytesPP()
	rowBytes := d.w * bpp
	if d.lineLen == fr.Stride && d.xoff == 0 {
		off := d.yoff * d.lineLen
		copy(d.mem[off:off+len(fr.Pix)], fr.Pix)
		return nil
	}
	for y := 0; y < d.h; y++ {
		off := (y+d.yoff)*d.lineLen + d.xoff*bpp
		copy(d.mem[off:off+rowBytes], fr.Pix[y*fr.Stride:y*fr.Stride+rowBytes])
	}
	return nil
}

func (d *fbDisplay) Close() error {
	if d.tty != nil {
		_ = ioctl(d.tty.Fd(), kdSetMode, kdText)
		d.tty.Close()
	}
	if d.mem != nil {
		_ = unix.Munmap(d.mem)
	}
	return d.f.Close()
}
