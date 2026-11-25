package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log/slog"
	"mime/multipart"
	"net/http/httptest"
	"testing"
)

func testApp(t *testing.T) *App {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	cfg := Config{DataDir: t.TempDir(), Workers: 1, JPEGQuality: 85, MaxUploadMB: 10, MaxZipMB: 50}
	events := NewBroker()
	store, err := OpenStore(cfg.DataDir, log, events)
	if err != nil {
		t.Fatal(err)
	}
	return &App{Cfg: cfg, Log: log, Store: store, Proc: NewProcessor(store, log, cfg, nil), Events: events}
}

func postFile(t *testing.T, a *App, name string, data []byte) []uploadResult {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write(data)
	mw.Close()
	req := httptest.NewRequest("POST", "/api/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.upload(rec, req)
	var res []uploadResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("response %d: %s", rec.Code, rec.Body)
	}
	return res
}

func makeImage(t *testing.T, enc func(*bytes.Buffer, image.Image) error, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, 255
	}
	var b bytes.Buffer
	if err := enc(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestZipUpload(t *testing.T) {
	a := testApp(t)
	jpg := func(b *bytes.Buffer, m image.Image) error { return jpeg.Encode(b, m, nil) }
	pngEnc := func(b *bytes.Buffer, m image.Image) error { return png.Encode(b, m) }
	red := makeImage(t, jpg, color.RGBA{R: 255})
	blue := makeImage(t, pngEnc, color.RGBA{B: 255})

	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	add := func(name string, data []byte, method uint16) {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method})
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
	}
	add("Photos-001/red.jpg", red, zip.Store)
	add("Photos-001/sub/blue.png", blue, zip.Deflate)
	add("Photos-001/red-copy.jpg", red, zip.Deflate) // same bytes: duplicate
	add("Photos-001/red.jpg.json", []byte(`{"title":"red"}`), zip.Deflate)
	add("Photos-001/clip.mp4", append([]byte{0, 0, 0, 0x18}, []byte("ftypmp42 not really a video")...), zip.Store)
	add("Photos-001/iphone.HEIC", append([]byte{0, 0, 0, 0x18}, []byte("ftypheic not really heic")...), zip.Store)
	add("__MACOSX/Photos-001/._red.jpg", []byte("junk"), zip.Store)
	add("Photos-001/empty/", nil, zip.Store)
	zw.Close()

	res := postFile(t, a, "Photos-001.zip", zbuf.Bytes())
	byName := map[string]uploadResult{}
	for _, r := range res {
		if r.Zip != "Photos-001.zip" {
			t.Errorf("%s: zip not set", r.Name)
		}
		byName[r.Name] = r
	}
	if len(res) != 4 {
		t.Fatalf("want 4 results (2 added, 1 duplicate, 1 HEIC error), got %+v", res)
	}
	if r := byName["red.jpg"]; r.Error != "" || r.Duplicate || r.Photo == nil {
		t.Errorf("red.jpg: %+v", r)
	}
	if r := byName["blue.png"]; r.Error != "" || r.Photo == nil || r.Photo.Ext != ".png" {
		t.Errorf("blue.png: %+v", r)
	}
	if r := byName["red-copy.jpg"]; !r.Duplicate {
		t.Errorf("red-copy.jpg should be a duplicate: %+v", r)
	}
	if r := byName["iphone.HEIC"]; r.Error == "" {
		t.Errorf("HEIC should be reported: %+v", r)
	}
	if n := len(a.Store.List()); n != 2 {
		t.Fatalf("store has %d photos, want 2", n)
	}
	if a.Proc.Pending() != 2 {
		t.Fatalf("queued %d photos for processing, want 2", a.Proc.Pending())
	}

	// Uploading the same zip again only finds duplicates.
	for _, r := range postFile(t, a, "again.zip", zbuf.Bytes()) {
		if r.Error == "" && !r.Duplicate {
			t.Errorf("second upload added %s", r.Name)
		}
	}
}

func TestZipUploadErrors(t *testing.T) {
	a := testApp(t)
	var empty bytes.Buffer
	zip.NewWriter(&empty).Close()
	if res := postFile(t, a, "empty.zip", empty.Bytes()); len(res) != 1 || res[0].Error != "no photos found in the zip" {
		t.Errorf("empty zip: %+v", res)
	}
	if res := postFile(t, a, "broken.zip", []byte("PK\x03\x04 truncated")); len(res) != 1 || res[0].Error == "" {
		t.Errorf("broken zip: %+v", res)
	}

	a.Cfg.MaxZipMB = 0
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, _ := zw.Create("a.jpg")
	w.Write(makeImage(t, func(b *bytes.Buffer, m image.Image) error { return jpeg.Encode(b, m, nil) }, color.RGBA{G: 255}))
	zw.Close()
	if res := postFile(t, a, "big.zip", zbuf.Bytes()); len(res) != 1 || res[0].Error == "" {
		t.Errorf("oversized zip: %+v", res)
	}
}
