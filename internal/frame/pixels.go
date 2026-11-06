package frame

import (
	"image"
	"image/draw"
	"runtime"
	"sync"
)

// PixelFormat describes the display's native memory layout. Frames are kept in
// this layout so showing one is a plain memcpy into the framebuffer.
type PixelFormat struct {
	BPP              int // 32 or 16 (RGB565)
	ROff, GOff, BOff int // byte offsets within a 32bpp pixel
}

// RGBX32 is a little-endian 32bpp layout used for headless mode.
var RGBX32 = PixelFormat{BPP: 32, ROff: 0, GOff: 1, BOff: 2}

func (f PixelFormat) bytesPP() int { return f.BPP / 8 }

// Frame is a screen-sized image in the display's native pixel format.
type Frame struct {
	W, H, Stride int
	Fmt          PixelFormat
	Pix          []byte
}

func NewFrame(w, h int, f PixelFormat) *Frame {
	stride := w * f.bytesPP()
	return &Frame{W: w, H: h, Stride: stride, Fmt: f, Pix: make([]byte, stride*h)}
}

// parallelRows splits [0,h) across all cores. The Pi 3 has 4, and every
// per-pixel loop in the frame goes through here.
func parallelRows(h int, fn func(y0, y1 int)) {
	n := runtime.GOMAXPROCS(0)
	if n <= 1 || h < 64 {
		fn(0, h)
		return
	}
	chunk := (h + n - 1) / n
	var wg sync.WaitGroup
	for y0 := 0; y0 < h; y0 += chunk {
		y1 := min(y0+chunk, h)
		wg.Add(1)
		go func() { defer wg.Done(); fn(y0, y1) }()
	}
	wg.Wait()
}

// FromImage converts img (which must be exactly W×H) into the frame. JPEGs
// decode to *image.YCbCr, which takes a fast direct-conversion path.
func (f *Frame) FromImage(img image.Image) {
	b := img.Bounds()
	switch src := img.(type) {
	case *image.YCbCr:
		f.fromYCbCr(src)
	case *image.RGBA:
		f.fromRGBALike(src.Pix, src.Stride, src.PixOffset(b.Min.X, b.Min.Y))
	case *image.NRGBA: // PNGs; alpha ignored (photos are opaque)
		f.fromRGBALike(src.Pix, src.Stride, src.PixOffset(b.Min.X, b.Min.Y))
	case *image.Gray:
		parallelRows(f.H, func(y0, y1 int) {
			rgb := make([]byte, f.W*3)
			for y := y0; y < y1; y++ {
				row := src.Pix[src.PixOffset(b.Min.X, b.Min.Y+y):]
				for x := 0; x < f.W; x++ {
					v := row[x]
					rgb[x*3], rgb[x*3+1], rgb[x*3+2] = v, v, v
				}
				f.packRow(y, rgb)
			}
		})
	default:
		rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)
		f.fromRGBALike(rgba.Pix, rgba.Stride, 0)
	}
}

func (f *Frame) fromRGBALike(pix []byte, stride, off int) {
	parallelRows(f.H, func(y0, y1 int) {
		rgb := make([]byte, f.W*3)
		for y := y0; y < y1; y++ {
			row := pix[off+y*stride:]
			for x := 0; x < f.W; x++ {
				rgb[x*3], rgb[x*3+1], rgb[x*3+2] = row[x*4], row[x*4+1], row[x*4+2]
			}
			f.packRow(y, rgb)
		}
	})
}

func (f *Frame) fromYCbCr(src *image.YCbCr) {
	hdiv, vdiv := 1, 1
	switch src.SubsampleRatio {
	case image.YCbCrSubsampleRatio422:
		hdiv = 2
	case image.YCbCrSubsampleRatio420:
		hdiv, vdiv = 2, 2
	case image.YCbCrSubsampleRatio440:
		vdiv = 2
	case image.YCbCrSubsampleRatio411:
		hdiv = 4
	case image.YCbCrSubsampleRatio410:
		hdiv, vdiv = 4, 2
	}
	b := src.Rect
	parallelRows(f.H, func(y0, y1 int) {
		rgb := make([]byte, f.W*3)
		for y := y0; y < y1; y++ {
			sy := b.Min.Y + y
			yrow := src.Y[(sy-b.Min.Y)*src.YStride:]
			crow := (sy/vdiv - b.Min.Y/vdiv) * src.CStride
			for x := 0; x < f.W; x++ {
				sx := b.Min.X + x
				ci := crow + sx/hdiv - b.Min.X/hdiv
				// Integer YCbCr→RGB, identical to image/color.YCbCrToRGB.
				yy1 := int32(yrow[x]) * 0x10101
				cb1 := int32(src.Cb[ci]) - 128
				cr1 := int32(src.Cr[ci]) - 128
				r := yy1 + 91881*cr1
				g := yy1 - 22554*cb1 - 46802*cr1
				bl := yy1 + 116130*cb1
				rgb[x*3] = clamp16(r)
				rgb[x*3+1] = clamp16(g)
				rgb[x*3+2] = clamp16(bl)
			}
			f.packRow(y, rgb)
		}
	})
}

func clamp16(v int32) byte {
	if uint32(v)&0xff000000 == 0 {
		return byte(v >> 16)
	}
	return byte(^(v >> 31))
}

// packRow writes one row of packed RGB triples in the native format.
func (f *Frame) packRow(y int, rgb []byte) {
	dst := f.Pix[y*f.Stride : y*f.Stride+f.W*f.Fmt.bytesPP()]
	if f.Fmt.BPP == 16 {
		for i, j := 0, 0; i < len(rgb); i, j = i+3, j+2 {
			v := uint16(rgb[i]>>3)<<11 | uint16(rgb[i+1]>>2)<<5 | uint16(rgb[i+2]>>3)
			dst[j], dst[j+1] = byte(v), byte(v>>8)
		}
		return
	}
	ro, gof, bo := f.Fmt.ROff, f.Fmt.GOff, f.Fmt.BOff
	xo := 6 - ro - gof - bo // the remaining byte (offsets are a permutation of 0..3)
	for i, j := 0, 0; i < len(rgb); i, j = i+3, j+4 {
		d := dst[j : j+4 : j+4]
		d[ro], d[gof], d[bo], d[xo] = rgb[i], rgb[i+1], rgb[i+2], 0xff
	}
}

// ToImage converts back to RGBA (used for the /api/screen preview).
func (f *Frame) ToImage() *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, f.W, f.H))
	parallelRows(f.H, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			src := f.Pix[y*f.Stride:]
			dst := out.Pix[y*out.Stride:]
			for x := 0; x < f.W; x++ {
				var r, g, b byte
				if f.Fmt.BPP == 16 {
					v := uint16(src[x*2]) | uint16(src[x*2+1])<<8
					r, g, b = byte(v>>11)<<3, byte(v>>5)<<2, byte(v)<<3
				} else {
					p := src[x*4:]
					r, g, b = p[f.Fmt.ROff], p[f.Fmt.GOff], p[f.Fmt.BOff]
				}
				dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = r, g, b, 0xff
			}
		}
	})
	return out
}

// Blend writes a*(256-alpha)/256 + b*alpha/256 into dst. All three frames
// must share dimensions and format.
func Blend(dst, a, b *Frame, alpha int) {
	alpha = min(max(alpha, 0), 256)
	inv := 256 - alpha
	if dst.Fmt.BPP == 16 {
		parallelRows(dst.H, func(y0, y1 int) {
			for i := y0 * dst.Stride; i < y1*dst.Stride; i += 2 {
				va := int(a.Pix[i]) | int(a.Pix[i+1])<<8
				vb := int(b.Pix[i]) | int(b.Pix[i+1])<<8
				r := ((va>>11)*inv + (vb>>11)*alpha) >> 8
				g := ((va>>5&63)*inv + (vb>>5&63)*alpha) >> 8
				bl := ((va&31)*inv + (vb&31)*alpha) >> 8
				v := r<<11 | g<<5 | bl
				dst.Pix[i], dst.Pix[i+1] = byte(v), byte(v>>8)
			}
		})
		return
	}
	parallelRows(dst.H, func(y0, y1 int) {
		lo, hi := y0*dst.Stride, y1*dst.Stride
		d, pa, pb := dst.Pix[lo:hi], a.Pix[lo:hi], b.Pix[lo:hi]
		for i := range d {
			d[i] = byte((int(pa[i])*inv + int(pb[i])*alpha) >> 8)
		}
	})
}
