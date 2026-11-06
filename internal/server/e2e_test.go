package server

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"photoframe/internal/frame"
	"photoframe/internal/logx"
)

// TestEndToEnd runs a headless frame and a server in-process and checks the
// full path: upload → process (with EXIF rotation) → push → delete.
func TestEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log, ring := logx.New(slog.LevelWarn, 100)
	const token = "secret"

	// Frame.
	frameDir := t.TempDir()
	fstore, err := frame.OpenStore(filepath.Join(frameDir, "photos"), log)
	if err != nil {
		t.Fatal(err)
	}
	disp := frame.NewHeadless(320, 180)
	show := frame.NewSlideshow(log, fstore, disp, frame.SettingsPath(frameDir), func() []string { return []string{"empty"} })
	fstore.SetOnChange(show.StoreChanged)
	go show.Run(ctx.Done())
	fapi := &frame.API{Log: log, Ring: ring, Store: fstore, Show: show, Display: disp, DataDir: frameDir, Token: token, Started: time.Now()}
	fsrv := httptest.NewServer(fapi.Handler())
	defer fsrv.Close()

	// Server.
	cfg := Config{DataDir: t.TempDir(), FrameURL: fsrv.URL, FrameToken: token, Workers: 2,
		JPEGQuality: 85, Background: "blur", SyncInterval: time.Hour, MaxUploadMB: 10}
	events := NewBroker()
	store, err := OpenStore(cfg.DataDir, log, events)
	if err != nil {
		t.Fatal(err)
	}
	var syncer *Syncer
	proc := NewProcessor(store, log, cfg, func(string) { syncer.Trigger() })
	syncer = NewSyncer(cfg, store, proc, events, log)
	proc.Start(ctx)
	go syncer.Run(ctx)
	app := &App{Cfg: cfg, Log: log, Ring: ring, Store: store, Proc: proc, Sync: syncer, Events: events}
	ssrv := httptest.NewServer(app.Handler())
	defer ssrv.Close()

	// Wrong token is rejected by the frame.
	if resp, _ := http.Get(fsrv.URL + "/api/info"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("frame accepted a request without token: %d", resp.StatusCode)
	}

	// The server should learn the frame's resolution.
	waitFor(t, "display size detection", func() bool { w, h := store.DisplaySize(); return w == 320 && h == 180 })

	// Upload a landscape 64x48 JPEG tagged "rotate 90° CW" → portrait on screen.
	src := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for i := range src.Pix {
		src.Pix[i] = 0xff
	}
	var raw bytes.Buffer
	jpeg.Encode(&raw, src, nil)
	data := withOrientation(raw.Bytes(), 6)

	res := upload(t, ssrv.URL, "test.jpg", data)
	if res.Error != "" || res.ID == "" {
		t.Fatalf("upload: %+v", res)
	}
	id := res.ID

	waitFor(t, "photo on frame", func() bool {
		m := fstore.Manifest()
		return len(m) == 1 && m[0].ID == id && m[0].Rev == "320x180v1"
	})
	p, _ := store.Get(id)
	if p.Width != 48 || p.Height != 64 {
		t.Fatalf("EXIF orientation not applied: %dx%d", p.Width, p.Height)
	}
	waitFor(t, "server marks synced", func() bool { p, _ := store.Get(id); return p.Synced })

	fp, _ := fstore.Get(id)
	f, _ := os.Open(fp.Path)
	cfgImg, err := jpeg.DecodeConfig(f)
	f.Close()
	if err != nil || cfgImg.Width != 320 || cfgImg.Height != 180 {
		t.Fatalf("frame copy should be exactly screen-sized: %+v %v", cfgImg, err)
	}
	waitFor(t, "slideshow shows it", func() bool { cur, _ := show.Status(); return cur == id })

	// Screenshot shows the white photo in the centre.
	if img := show.Screenshot(); img == nil || colorDist(img.RGBAAt(160, 90), color.RGBA{255, 255, 255, 255}) > 30 {
		t.Fatalf("screenshot centre pixel wrong")
	}

	// Re-uploading identical bytes is a no-op.
	if res := upload(t, ssrv.URL, "copy.jpg", data); !res.Duplicate || res.ID != id {
		t.Fatalf("expected duplicate: %+v", res)
	}

	// Deleting on the server removes it from the frame.
	req, _ := http.NewRequest("DELETE", ssrv.URL+"/api/photos/"+id, nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %v %v", resp, err)
	}
	waitFor(t, "removed from frame", func() bool { return fstore.Count() == 0 })
	waitFor(t, "slideshow empty", func() bool { cur, _ := show.Status(); return cur == "" })

	// Proxied frame logs are reachable through the server.
	resp, err := http.Get(ssrv.URL + "/api/frame/logs?level=debug")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("frame logs proxy: %v %v", resp, err)
	}
	resp.Body.Close()
}

func colorDist(a, b color.RGBA) int {
	d := func(x, y uint8) int { return max(int(x)-int(y), int(y)-int(x)) }
	return d(a.R, b.R) + d(a.G, b.G) + d(a.B, b.B)
}

func withOrientation(jpg []byte, o byte) []byte {
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 0x01, 3, 0, 1, 0, 0, 0, o, 0, 0, 0, 0, 0, 0, 0}
	seg := append([]byte("Exif\x00\x00"), tiff...)
	out := []byte{0xFF, 0xD8, 0xFF, 0xE1, byte((len(seg) + 2) >> 8), byte(len(seg) + 2)}
	out = append(out, seg...)
	return append(out, jpg[2:]...)
}

func upload(t *testing.T, base, name string, data []byte) uploadResult {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write(data)
	mw.Close()
	resp, err := http.Post(base+"/api/upload", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var res []uploadResult
	if err := json.Unmarshal(b, &res); err != nil || len(res) != 1 {
		t.Fatalf("upload response %d: %s", resp.StatusCode, b)
	}
	return res[0]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
