package logx

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

func TestRingWrapsAndFilters(t *testing.T) {
	r := NewRing(3)
	log := slog.New(&ringHandler{ring: r, level: slog.LevelDebug}).With("component", "test")
	log.Debug("one")
	log.Info("two", "n", 2)
	log.Warn("three", "err", errors.New("boom"))
	log.WithGroup("g").Error("four", "k", "v")

	all, next := r.Since(0, slog.LevelDebug, 0)
	if next != 4 || len(all) != 3 || all[0].Msg != "two" {
		t.Fatalf("expected the oldest entry to be evicted: next=%d %+v", next, all)
	}
	if all[1].Attrs["err"] != "boom" || all[1].Attrs["component"] != "test" {
		t.Fatalf("attrs: %+v", all[1].Attrs)
	}
	if all[2].Attrs["g.k"] != "v" {
		t.Fatalf("group attrs: %+v", all[2].Attrs)
	}
	if warn, _ := r.Since(0, slog.LevelWarn, 0); len(warn) != 2 {
		t.Fatalf("level filter: %+v", warn)
	}
	if newer, _ := r.Since(3, slog.LevelDebug, 0); len(newer) != 1 || newer[0].Msg != "four" {
		t.Fatalf("since filter: %+v", newer)
	}
	if !(&ringHandler{level: slog.LevelInfo}).Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("enabled")
	}
}
