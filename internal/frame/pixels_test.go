package frame

import (
	"image"
	"image/color"
	"testing"
)

func TestFromYCbCrMatchesStdlib(t *testing.T) {
	for _, ratio := range []image.YCbCrSubsampleRatio{
		image.YCbCrSubsampleRatio444, image.YCbCrSubsampleRatio422, image.YCbCrSubsampleRatio420,
	} {
		src := image.NewYCbCr(image.Rect(0, 0, 37, 23), ratio)
		for i := range src.Y {
			src.Y[i] = uint8(i * 7)
		}
		for i := range src.Cb {
			src.Cb[i], src.Cr[i] = uint8(i*13), uint8(255-i*5)
		}
		for _, f := range []PixelFormat{RGBX32, {BPP: 32, ROff: 2, GOff: 1, BOff: 0}} {
			fr := NewFrame(37, 23, f)
			fr.FromImage(src)
			out := fr.ToImage()
			for y := 0; y < 23; y++ {
				for x := 0; x < 37; x++ {
					yc := src.YCbCrAt(x, y)
					r, g, b := color.YCbCrToRGB(yc.Y, yc.Cb, yc.Cr)
					want := color.RGBA{r, g, b, 255}
					if got := out.RGBAAt(x, y); got != want {
						t.Fatalf("ratio %v fmt %+v at %d,%d: got %v want %v", ratio, f, x, y, got, want)
					}
				}
			}
		}
	}
}

func TestRGB565RoundTrip(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 1))
	src.SetRGBA(0, 0, color.RGBA{255, 0, 0, 255})
	src.SetRGBA(1, 0, color.RGBA{0, 255, 0, 255})
	src.SetRGBA(2, 0, color.RGBA{0, 0, 255, 255})
	src.SetRGBA(3, 0, color.RGBA{255, 255, 255, 255})
	fr := NewFrame(4, 1, PixelFormat{BPP: 16})
	fr.FromImage(src)
	out := fr.ToImage()
	want := []color.RGBA{{248, 0, 0, 255}, {0, 252, 0, 255}, {0, 0, 248, 255}, {248, 252, 248, 255}}
	for x, w := range want {
		if got := out.RGBAAt(x, 0); got != w {
			t.Errorf("pixel %d: got %v want %v", x, got, w)
		}
	}
}

func TestBlend(t *testing.T) {
	a, b, d := NewFrame(2, 2, RGBX32), NewFrame(2, 2, RGBX32), NewFrame(2, 2, RGBX32)
	for i := range b.Pix {
		b.Pix[i] = 200
	}
	Blend(d, a, b, 0)
	if d.Pix[0] != 0 {
		t.Errorf("alpha 0: got %d", d.Pix[0])
	}
	Blend(d, a, b, 128)
	if d.Pix[0] != 100 {
		t.Errorf("alpha 128: got %d", d.Pix[0])
	}
	Blend(d, a, b, 256)
	if d.Pix[0] != 200 {
		t.Errorf("alpha 256: got %d", d.Pix[0])
	}
}

func BenchmarkFromYCbCr1080p(b *testing.B) {
	src := image.NewYCbCr(image.Rect(0, 0, 1920, 1080), image.YCbCrSubsampleRatio420)
	fr := NewFrame(1920, 1080, PixelFormat{BPP: 32, ROff: 2, GOff: 1, BOff: 0})
	for b.Loop() {
		fr.FromImage(src)
	}
}

func BenchmarkBlend1080p(b *testing.B) {
	f := PixelFormat{BPP: 32, ROff: 2, GOff: 1, BOff: 0}
	a, c, d := NewFrame(1920, 1080, f), NewFrame(1920, 1080, f), NewFrame(1920, 1080, f)
	for b.Loop() {
		Blend(d, a, c, 100)
	}
}
