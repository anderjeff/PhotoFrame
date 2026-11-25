package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"photoframe/internal/api"
	"photoframe/internal/frame"
	"photoframe/internal/logx"
)

// jpegWithDate builds a small JPEG whose EXIF has DateTimeOriginal = d (in a
// sub-IFD, as cameras write it) or, with inIFD0, DateTime in IFD0.
func jpegWithDate(t *testing.T, d time.Time, c color.RGBA, inIFD0 bool) []byte {
	t.Helper()
	bo := binary.LittleEndian
	str := append([]byte(d.Format("2006:01:02 15:04:05")), 0)
	tiff := []byte("II")
	tiff = bo.AppendUint16(tiff, 42)
	tiff = bo.AppendUint32(tiff, 8)
	entry := func(b []byte, tag, typ uint16, count, val uint32) []byte {
		b = bo.AppendUint16(b, tag)
		b = bo.AppendUint16(b, typ)
		b = bo.AppendUint32(b, count)
		return bo.AppendUint32(b, val)
	}
	if inIFD0 {
		tiff = bo.AppendUint16(tiff, 1)
		tiff = entry(tiff, 0x0132, 2, 20, 26)
		tiff = bo.AppendUint32(tiff, 0)
	} else {
		tiff = bo.AppendUint16(tiff, 1)
		tiff = entry(tiff, 0x8769, 4, 1, 26) // Exif IFD at 26
		tiff = bo.AppendUint32(tiff, 0)
		tiff = bo.AppendUint16(tiff, 1)
		tiff = entry(tiff, 0x9003, 2, 20, 44)
		tiff = bo.AppendUint32(tiff, 0)
	}
	tiff = append(tiff, str...)
	seg := append([]byte("Exif\x00\x00"), tiff...)

	img := image.NewRGBA(image.Rect(0, 0, 16, 12))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, 255
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	out := []byte{0xFF, 0xD8, 0xFF, 0xE1, byte((len(seg) + 2) >> 8), byte(len(seg) + 2)}
	out = append(out, seg...)
	return append(out, buf.Bytes()[2:]...)
}

func TestExifDateTaken(t *testing.T) {
	d := time.Date(2017, 10, 21, 14, 3, 9, 0, time.UTC)
	for _, ifd0 := range []bool{false, true} {
		data := jpegWithDate(t, d, color.RGBA{R: 200}, ifd0)
		if got := exifDateTaken(data); !got.Equal(d) {
			t.Errorf("ifd0=%v: got %v", ifd0, got)
		}
		if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
			t.Fatalf("test JPEG invalid: %v", err)
		}
		if o := exifOrientation(data); o != 1 {
			t.Errorf("orientation without tag: %d", o)
		}
	}
	if got := exifDateTaken(jpegWithOrientation(t, 6, true)); !got.IsZero() {
		t.Errorf("no date: got %v", got)
	}
	if got := exifDateTaken([]byte("garbage")); !got.IsZero() {
		t.Errorf("garbage: got %v", got)
	}
}

func TestYearsAgo(t *testing.T) {
	day := func(s string) time.Time { d, _ := time.Parse("2006-01-02", s); return d }
	cases := []struct {
		taken, today string
		window, ago  int
		ok           bool
	}{
		{"2017-11-25", "2026-11-25", 0, 9, true},
		{"2017-11-24", "2026-11-25", 0, 0, false},
		{"2017-11-22", "2026-11-25", 3, 9, true},
		{"2017-11-29", "2026-11-25", 3, 0, false},
		{"2026-11-25", "2026-11-25", 3, 0, false}, // today isn't a memory
		{"2026-11-23", "2026-11-25", 3, 0, false},
		{"2024-12-31", "2026-01-01", 2, 1, true}, // across New Year
		{"2025-12-31", "2026-01-01", 2, 0, false},
		{"2024-02-29", "2026-03-01", 0, 2, true}, // leap day lands on Mar 1
	}
	for _, c := range cases {
		ago, ok := yearsAgo(day(c.taken), day(c.today), c.window)
		if ok != c.ok || ago != c.ago {
			t.Errorf("%s on %s ±%d: got %d %v", c.taken, c.today, c.window, ago, ok)
		}
	}
}

func TestOnThisDayToday(t *testing.T) {
	now := time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC) // evening of the 24th in Chicago
	o := OnThisDay{TimeZone: "America/Chicago"}
	if got := o.Today(now).Format("2006-01-02"); got != "2026-11-24" {
		t.Errorf("Chicago: %s", got)
	}
	o = OnThisDay{TimeZone: "Not/AZone"}
	o.Normalize()
	if o.TimeZone != "" {
		t.Errorf("invalid zone kept: %q", o.TimeZone)
	}
}

// TestOnThisDayEndToEnd enables the feature and checks that the frame shows
// today's photos from earlier years next, oldest first.
func TestOnThisDayEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log, ring := logx.New(slog.LevelWarn, 100)

	frameDir := t.TempDir()
	fstore, err := frame.OpenStore(filepath.Join(frameDir, "photos"), log)
	if err != nil {
		t.Fatal(err)
	}
	disp := frame.NewHeadless(320, 180)
	show := frame.NewSlideshow(log, fstore, disp, frame.SettingsPath(frameDir), func() []string { return nil })
	if _, err := show.SetSettings(api.Settings{IntervalSeconds: 3600, Transition: "none"}); err != nil {
		t.Fatal(err)
	}
	fstore.SetOnChange(show.StoreChanged)
	go show.Run(ctx.Done())
	fapi := &frame.API{Log: log, Ring: ring, Store: fstore, Show: show, Display: disp, DataDir: frameDir, Started: time.Now()}
	fsrv := httptest.NewServer(fapi.Handler())
	defer fsrv.Close()

	cfg := Config{DataDir: t.TempDir(), FrameURL: fsrv.URL, Workers: 2, JPEGQuality: 85, SyncInterval: time.Hour, MaxUploadMB: 10}
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

	// An undated photo goes on screen first; with a 1-hour interval it stays.
	plain := upload(t, ssrv.URL, "plain.jpg", makeImage(t, func(b *bytes.Buffer, m image.Image) error { return jpeg.Encode(b, m, nil) }, color.RGBA{G: 255}))
	waitFor(t, "plain photo shown", func() bool { cur, _ := show.Status(); return cur == plain.ID })

	now := time.Now().UTC()
	threeYears := upload(t, ssrv.URL, "3y.jpg", jpegWithDate(t, now.AddDate(-3, 0, 0), color.RGBA{R: 255}, false))
	fiveYears := upload(t, ssrv.URL, "5y.jpg", jpegWithDate(t, now.AddDate(-5, 0, 1), color.RGBA{B: 255}, false))
	lastMonth := upload(t, ssrv.URL, "recent.jpg", jpegWithDate(t, now.AddDate(0, -1, 0), color.RGBA{R: 255, G: 255}, false))
	for _, r := range []uploadResult{threeYears, fiveYears, lastMonth} {
		if r.Error != "" {
			t.Fatalf("upload: %+v", r)
		}
	}
	waitFor(t, "all synced", func() bool {
		return len(fstore.Manifest()) == 4 && len(OnThisDayPhotos(store.List(), OnThisDay{TimeZone: "UTC"}.Today(now), 3)) == 2
	})
	waitFor(t, "server marks synced", func() bool {
		for _, p := range store.List() {
			if !p.Synced {
				return false
			}
		}
		return true
	})
	if p, _ := store.Get(threeYears.ID); !p.DateRead || p.TakenAt.Year() != now.Year()-3 {
		t.Fatalf("date taken not recorded: %+v", p)
	}

	body := `{"enabled":true,"windowDays":3,"repeatMinutes":60,"timeZone":"UTC"}`
	req, _ := http.NewRequest("PUT", ssrv.URL+"/api/onthisday", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("PUT onthisday: %v %v", resp, err)
	}
	var st onThisDayStatus
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if len(st.Photos) != 2 || st.Photos[0].ID != fiveYears.ID || st.Photos[0].YearsAgo != 5 || st.Dated != 3 || st.Total != 4 {
		t.Fatalf("status: %+v", st)
	}

	waitFor(t, "frame receives the group", func() bool { return show.FeaturedKey() != "" })
	for _, want := range []string{fiveYears.ID, threeYears.ID} {
		show.Control("next")
		waitFor(t, "group photo shown", func() bool { cur, _ := show.Status(); return cur == want })
	}

	// Turning it off clears the frame's group.
	req, _ = http.NewRequest("PUT", ssrv.URL+"/api/onthisday", strings.NewReader(`{"enabled":false}`))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	waitFor(t, "group cleared", func() bool { return show.FeaturedKey() == "off" })
}
