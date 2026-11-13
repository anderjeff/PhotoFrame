package server

import (
	"image"
	"log/slog"
	"testing"

	"photoframe/internal/api"
)

func TestBackgroundTag(t *testing.T) {
	for _, tc := range []struct {
		b    Background
		want string
	}{
		{DefaultBackground(""), ""}, // must stay empty: revs from before this setting remain valid
		{Background{Mode: "blur", Color: "#123456", Brightness: 55, Blur: 5}, ""},
		{Background{Mode: "blur", Brightness: 30, Blur: 8}, "b30s8"},
		{DefaultBackground("black"), "c000000"},
		{Background{Mode: "color", Color: "#F2F0EB"}, "cf2f0eb"},
	} {
		b := tc.b
		if err := b.Normalize(); err != nil {
			t.Fatal(err)
		}
		if got := b.tag(); got != tc.want {
			t.Errorf("%+v: tag %q, want %q", tc.b, got, tc.want)
		}
		if rev := revFor(1920, 1080, b); !api.ValidRev(rev) {
			t.Errorf("invalid rev %q", rev)
		}
	}
}

func TestRevBase(t *testing.T) {
	for in, want := range map[string]string{
		"1920x1080v1": "1920x1080v1", "1920x1080v1c000000": "1920x1080v1",
		"1920x1080v12b30s8": "1920x1080v12", "": "",
	} {
		if got := revBase(in); got != want {
			t.Errorf("revBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBackgroundNormalize(t *testing.T) {
	for _, bad := range []Background{{Mode: "stripes"}, {Mode: "color", Color: "red"}, {Mode: "color", Color: "#12345g"}} {
		if err := bad.Normalize(); err == nil {
			t.Errorf("%+v: expected error", bad)
		}
	}
	b := Background{Mode: " Blur ", Brightness: 150, Blur: 0}
	if err := b.Normalize(); err != nil || b.Mode != "blur" || b.Brightness != 100 || b.Blur != 1 || b.Color != "#000000" {
		t.Fatalf("normalize: %+v %v", b, err)
	}
}

func TestRenderSolidColor(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 30, 60)) // portrait on a landscape canvas
	bg := Background{Mode: "color", Color: "#336699"}
	canvas, _ := render(src, 1, 320, 180, bg)
	if c := canvas.RGBAAt(2, 90); c.R != 0x33 || c.G != 0x66 || c.B != 0x99 {
		t.Fatalf("edge pixel %v, want #336699", c)
	}
}

func TestStoreBackgroundPersists(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	s, err := OpenStore(dir, log, NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	s.SetDefaultBackground(DefaultBackground("black"))
	if s.Background().Mode != "color" {
		t.Fatal("default from env not used")
	}
	want := Background{Mode: "blur", Color: "#000000", Brightness: 40, Blur: 7}
	if !s.SetBackground(want) {
		t.Fatal("expected change")
	}
	if s.SetBackground(want) {
		t.Fatal("same background reported as a change")
	}
	s.save()

	s2, err := OpenStore(dir, log, NewBroker())
	if err != nil {
		t.Fatal(err)
	}
	s2.SetDefaultBackground(DefaultBackground("black")) // a saved background wins over the env default
	if got := s2.Background(); got != want {
		t.Fatalf("reloaded %+v, want %+v", got, want)
	}
}
