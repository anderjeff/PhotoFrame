package server

import (
	"bufio"
	"image"
	"image/draw"
	"image/jpeg"
	"os"
	"path/filepath"

	xdraw "golang.org/x/image/draw"
)

// fitSize returns the largest size with w:h aspect that fits in maxW×maxH.
func fitSize(w, h, maxW, maxH int) (int, int) {
	if w <= 0 || h <= 0 {
		return maxW, maxH
	}
	if w*maxH > h*maxW { // wider than target
		return maxW, max(1, min(maxH, (h*maxW+w/2)/w))
	}
	return max(1, min(maxW, (w*maxH+h/2)/h)), maxH
}

// orient applies an EXIF orientation transform to src.
func orient(src *image.RGBA, o int) *image.RGBA {
	if o < 2 || o > 8 {
		return src
	}
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	dw, dh := sw, sh
	if o >= 5 {
		dw, dh = sh, sw
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sx, sy int
			switch o {
			case 2:
				sx, sy = sw-1-x, y
			case 3:
				sx, sy = sw-1-x, sh-1-y
			case 4:
				sx, sy = x, sh-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, sh-1-x
			case 7:
				sx, sy = sw-1-y, sh-1-x
			case 8:
				sx, sy = sw-1-y, x
			}
			si := sy*src.Stride + sx*4
			di := y*dst.Stride + x*4
			copy(dst.Pix[di:di+4], src.Pix[si:si+4])
		}
	}
	return dst
}

// render produces the exact W×H image the frame displays, plus the fitted
// (un-padded) photo used for the thumbnail. The source is scaled before it is
// rotated, so the expensive full-resolution work happens only once.
func render(img image.Image, orientation, W, H int, bg Background) (canvas, fitted *image.RGBA) {
	sb := img.Bounds()
	ow, oh := sb.Dx(), sb.Dy()
	if orientation >= 5 {
		ow, oh = oh, ow
	}
	fw, fh := fitSize(ow, oh, W, H)
	tw, th := fw, fh
	if orientation >= 5 {
		tw, th = fh, fw
	}
	scaled := image.NewRGBA(image.Rect(0, 0, tw, th))
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), img, sb, xdraw.Src, nil)
	fitted = orient(scaled, orientation)

	if fw == W && fh == H {
		return fitted, fitted
	}
	canvas = image.NewRGBA(image.Rect(0, 0, W, H))
	if bg.Mode == "color" {
		draw.Draw(canvas, canvas.Bounds(), image.NewUniform(bg.rgba()), image.Point{}, draw.Src)
	} else {
		blurBackground(canvas, fitted, bg.Brightness, bg.Blur)
	}
	x0, y0 := (W-fw)/2, (H-fh)/2
	draw.Draw(canvas, image.Rect(x0, y0, x0+fw, y0+fh), fitted, image.Point{}, draw.Src)
	return canvas, fitted
}

// blurBackground fills canvas with a darkened, blurred, cover-cropped copy of
// src, so portrait photos on a landscape screen don't get black bars.
// brightness is 0–100 %; strength 1–10 sets how small the copy is shrunk
// before being blown back up (5 = 1/24 of the screen size).
func blurBackground(canvas, src *image.RGBA, brightness, strength int) {
	W, H := canvas.Rect.Dx(), canvas.Rect.Dy()
	div := 4*strength + 4
	sw, sh := max(W/div, 1), max(H/div, 1)
	small := image.NewRGBA(image.Rect(0, 0, sw, sh))
	xdraw.BiLinear.Scale(small, small.Bounds(), src, coverRect(src.Rect, sw, sh), xdraw.Src, nil)
	for range 3 {
		boxBlur(small, 2)
	}
	for i := 0; i < len(small.Pix); i += 4 {
		small.Pix[i] = byte(int(small.Pix[i]) * brightness / 100)
		small.Pix[i+1] = byte(int(small.Pix[i+1]) * brightness / 100)
		small.Pix[i+2] = byte(int(small.Pix[i+2]) * brightness / 100)
	}
	xdraw.BiLinear.Scale(canvas, canvas.Bounds(), small, small.Bounds(), xdraw.Src, nil)
}

// coverRect is the centered sub-rectangle of r with aspect tw:th.
func coverRect(r image.Rectangle, tw, th int) image.Rectangle {
	w, h := r.Dx(), r.Dy()
	if w*th > h*tw {
		cw := h * tw / th
		x0 := r.Min.X + (w-cw)/2
		return image.Rect(x0, r.Min.Y, x0+cw, r.Max.Y)
	}
	ch := w * th / tw
	y0 := r.Min.Y + (h-ch)/2
	return image.Rect(r.Min.X, y0, r.Max.X, y0+ch)
}

func boxBlur(img *image.RGBA, r int) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	tmp := make([]byte, len(img.Pix))
	pass := func(src, dst []byte, n, m int, idx func(i, j int) int) {
		for j := 0; j < m; j++ {
			for i := 0; i < n; i++ {
				var sum [3]int
				for k := -r; k <= r; k++ {
					p := idx(min(max(i+k, 0), n-1), j)
					sum[0] += int(src[p])
					sum[1] += int(src[p+1])
					sum[2] += int(src[p+2])
				}
				d := idx(i, j)
				dst[d], dst[d+1], dst[d+2], dst[d+3] = byte(sum[0]/(2*r+1)), byte(sum[1]/(2*r+1)), byte(sum[2]/(2*r+1)), 255
			}
		}
	}
	pass(img.Pix, tmp, w, h, func(x, y int) int { return y*img.Stride + x*4 })
	pass(tmp, img.Pix, h, w, func(y, x int) int { return y*img.Stride + x*4 })
}

// thumbnail scales img to fit within size×size.
func thumbnail(img *image.RGBA, size int) *image.RGBA {
	tw, th := fitSize(img.Rect.Dx(), img.Rect.Dy(), size, size)
	if tw >= img.Rect.Dx() {
		return img
	}
	out := image.NewRGBA(image.Rect(0, 0, tw, th))
	xdraw.BiLinear.Scale(out, out.Bounds(), img, img.Rect, xdraw.Src, nil)
	return out
}

// writeJPEG encodes atomically: readers never see a partial file.
func writeJPEG(path string, img image.Image, quality int) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*.jpg")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	bw := bufio.NewWriterSize(tmp, 256<<10)
	err = jpeg.Encode(bw, img, &jpeg.Options{Quality: quality})
	if err == nil {
		err = bw.Flush()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
