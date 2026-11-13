// Command photoframe runs on the Raspberry Pi: it shows photos from the SD
// card on the framebuffer and exposes an HTTP API the NAS server syncs to.
//
// Every flag can also be set by environment variable (shown in brackets);
// the installer writes these to /etc/photoframe/frame.env.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"photoframe/internal/frame"
	"photoframe/internal/logx"
	"photoframe/internal/version"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func main() {
	var (
		listen   = flag.String("listen", env("LISTEN", ":8080"), "HTTP listen address [LISTEN]")
		dataDir  = flag.String("data", env("DATA_DIR", "/var/lib/photoframe"), "data directory [DATA_DIR]")
		token    = flag.String("token", env("FRAME_TOKEN", ""), "shared secret required from the server [FRAME_TOKEN]")
		fbDev    = flag.String("fb", env("FB_DEVICE", "/dev/fb0"), "framebuffer device [FB_DEVICE]")
		tty      = flag.String("tty", env("TTY", "/dev/tty1"), "console tty to switch to graphics mode; empty to skip [TTY]")
		headless = flag.Bool("headless", env("HEADLESS", "") == "1", "don't use the framebuffer (development) [HEADLESS=1]")
		hw       = flag.Int("headless-width", envInt("HEADLESS_WIDTH", 1920), "screen width in headless mode")
		hh       = flag.Int("headless-height", envInt("HEADLESS_HEIGHT", 1080), "screen height in headless mode")
		level    = flag.String("log-level", env("LOG_LEVEL", "info"), "debug, info, warn, error [LOG_LEVEL]")
		showVer  = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version.Version)
		return
	}

	log, ring := logx.New(logx.ParseLevel(*level, slog.LevelInfo), 5000)
	log.Info("photoframe starting", "version", version.Version, "data", *dataDir)
	if *token == "" {
		log.Warn("FRAME_TOKEN is empty: the API accepts unauthenticated requests")
	}

	store, err := frame.OpenStore(filepath.Join(*dataDir, "photos"), log)
	if err != nil {
		log.Error("cannot open photo directory", "err", err)
		os.Exit(1)
	}

	var disp frame.Display
	if !*headless {
		disp, err = frame.OpenFramebuffer(*fbDev, *tty)
		if err != nil {
			log.Error("framebuffer unavailable; running headless so the API stays reachable", "device", *fbDev, "err", err)
		}
	}
	if disp == nil {
		disp = frame.NewHeadless(*hw, *hh)
	}
	defer disp.Close()
	w, h := disp.Size()
	log.Info("display ready", "display", disp.Name(), "width", w, "height", h, "photos", store.Count())

	port := *listen
	if _, p, err := net.SplitHostPort(*listen); err == nil {
		port = ":" + p
	}
	emptyLines := func() []string {
		lines := []string{"PhotoFrame", "", "No photos yet.", "Upload photos from the PhotoFrame server.", ""}
		for _, a := range frame.LocalAddresses() {
			lines = append(lines, "This frame: "+a+port)
		}
		return lines
	}
	show := frame.NewSlideshow(log, store, disp, frame.SettingsPath(*dataDir), emptyLines)
	store.SetOnChange(show.StoreChanged)
	store.SetOnReplace(show.StoreReplaced)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	showDone := make(chan struct{})
	go func() { defer close(showDone); show.Run(ctx.Done()) }()

	// Pick up files added or removed on the SD card by hand.
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := store.Scan(); err != nil {
					log.Warn("rescan failed", "err", err)
				}
			}
		}
	}()

	a := &frame.API{Log: log, Ring: ring, Store: store, Show: show, Display: disp,
		DataDir: *dataDir, Token: *token, Started: time.Now()}
	srv := &http.Server{Addr: *listen, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("api listening", "addr", *listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	<-showDone
}
