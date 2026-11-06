package server

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// jpegWithOrientation builds a minimal JPEG whose APP1 carries an EXIF orientation tag.
func jpegWithOrientation(t *testing.T, o uint16, bigEndian bool) []byte {
	t.Helper()
	var bo binary.AppendByteOrder = binary.LittleEndian
	tiff := []byte("II")
	if bigEndian {
		bo, tiff = binary.BigEndian, []byte("MM")
	}
	tiff = bo.AppendUint16(tiff, 42)
	tiff = bo.AppendUint32(tiff, 8)
	tiff = bo.AppendUint16(tiff, 1)      // one entry
	tiff = bo.AppendUint16(tiff, 0x0112) // orientation
	tiff = bo.AppendUint16(tiff, 3)      // SHORT
	tiff = bo.AppendUint32(tiff, 1)
	tiff = bo.AppendUint16(tiff, o)
	tiff = bo.AppendUint16(tiff, 0)
	tiff = bo.AppendUint32(tiff, 0) // next IFD
	seg := append([]byte("Exif\x00\x00"), tiff...)

	var img bytes.Buffer
	if err := jpeg.Encode(&img, image.NewGray(image.Rect(0, 0, 4, 4)), nil); err != nil {
		t.Fatal(err)
	}
	b := img.Bytes()
	out := []byte{0xFF, 0xD8, 0xFF, 0xE1, byte((len(seg) + 2) >> 8), byte(len(seg) + 2)}
	out = append(out, seg...)
	return append(out, b[2:]...)
}

func TestExifOrientation(t *testing.T) {
	for _, be := range []bool{false, true} {
		for o := uint16(1); o <= 8; o++ {
			data := jpegWithOrientation(t, o, be)
			if got := exifOrientation(data); got != int(o) {
				t.Errorf("bigEndian=%v orientation %d: got %d", be, o, got)
			}
			if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
				t.Fatalf("test JPEG invalid: %v", err)
			}
		}
	}
	if got := exifOrientation([]byte("not a jpeg")); got != 1 {
		t.Errorf("garbage: got %d", got)
	}
	if got := exifOrientation([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0xFF}); got != 1 {
		t.Errorf("truncated: got %d", got)
	}
}

func TestOrient(t *testing.T) {
	// 3x2 source with distinct pixel values:
	//  0 1 2
	//  3 4 5
	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	for i := range 6 {
		src.Set(i%3, i/3, color.RGBA{uint8(i), 0, 0, 255})
	}
	read := func(img *image.RGBA) [][]uint8 {
		var rows [][]uint8
		for y := 0; y < img.Rect.Dy(); y++ {
			var r []uint8
			for x := 0; x < img.Rect.Dx(); x++ {
				r = append(r, img.RGBAAt(x, y).R)
			}
			rows = append(rows, r)
		}
		return rows
	}
	want := map[int][][]uint8{
		1: {{0, 1, 2}, {3, 4, 5}},
		2: {{2, 1, 0}, {5, 4, 3}},
		3: {{5, 4, 3}, {2, 1, 0}},
		4: {{3, 4, 5}, {0, 1, 2}},
		5: {{0, 3}, {1, 4}, {2, 5}},
		6: {{3, 0}, {4, 1}, {5, 2}}, // rotate 90° clockwise
		7: {{5, 2}, {4, 1}, {3, 0}},
		8: {{2, 5}, {1, 4}, {0, 3}}, // rotate 90° counter-clockwise
	}
	for o, w := range want {
		got := read(orient(src, o))
		if len(got) != len(w) {
			t.Fatalf("orientation %d: got %v want %v", o, got, w)
		}
		for y := range w {
			for x := range w[y] {
				if got[y][x] != w[y][x] {
					t.Fatalf("orientation %d: got %v want %v", o, got, w)
				}
			}
		}
	}
}

func TestFitSize(t *testing.T) {
	cases := []struct{ w, h, mw, mh, ew, eh int }{
		{4000, 3000, 1920, 1080, 1440, 1080}, // 4:3 into 16:9
		{3000, 4000, 1920, 1080, 810, 1080},  // portrait
		{3840, 2160, 1920, 1080, 1920, 1080}, // exact aspect
		{6000, 1000, 1920, 1080, 1920, 320},  // panorama
		{100, 50, 1920, 1080, 1920, 960},     // upscales small images
	}
	for _, c := range cases {
		if w, h := fitSize(c.w, c.h, c.mw, c.mh); w != c.ew || h != c.eh {
			t.Errorf("fitSize(%d,%d,%d,%d) = %dx%d, want %dx%d", c.w, c.h, c.mw, c.mh, w, h, c.ew, c.eh)
		}
	}
}

func TestRenderOutputsExactScreenSize(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 300, 400)) // portrait
	for _, o := range []int{1, 6} {
		canvas, fitted := render(src, o, 320, 180, "blur")
		if canvas.Rect.Dx() != 320 || canvas.Rect.Dy() != 180 {
			t.Errorf("orientation %d: canvas %v", o, canvas.Rect)
		}
		ow, oh := 300, 400
		if o == 6 {
			ow, oh = 400, 300
		}
		fw, fh := fitSize(ow, oh, 320, 180)
		if fitted.Rect.Dx() != fw || fitted.Rect.Dy() != fh {
			t.Errorf("orientation %d: fitted %v, want %dx%d", o, fitted.Rect, fw, fh)
		}
	}
}
