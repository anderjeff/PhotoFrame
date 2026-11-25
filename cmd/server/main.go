// Command photoframe-server runs on the NAS (in Docker). It hosts the upload
// web UI, renders photos for the frame's screen, and syncs them to the frame.
// Configuration is via environment variables; see internal/server/config.go.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // honour TZ in the distroless image

	"photoframe/internal/logx"
	"photoframe/internal/server"
	"photoframe/internal/version"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local server and exit (for Docker HEALTHCHECK)")
	showVer := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	cfg := server.ConfigFromEnv()
	if *showVer {
		fmt.Println(version.Version)
		return
	}
	if *healthcheck {
		os.Exit(probe(cfg.Listen))
	}

	log, ring := logx.New(cfg.LogLevel, 5000)
	log.Info("photoframe-server starting", "version", version.Version, "data", cfg.DataDir,
		"frame", cfg.FrameURL, "workers", cfg.Workers)
	if cfg.FrameToken == "" {
		log.Warn("FRAME_TOKEN is empty; it must match the token configured on the frame")
	}

	events := server.NewBroker()
	store, err := server.OpenStore(cfg.DataDir, log, events)
	if err != nil {
		log.Error("cannot open data directory", "err", err)
		os.Exit(1)
	}
	if cfg.Width > 0 && cfg.Height > 0 {
		store.SetDisplaySize(cfg.Width, cfg.Height)
	}
	store.SetDefaultBackground(server.DefaultBackground(cfg.Background))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var syncer *server.Syncer
	proc := server.NewProcessor(store, log, cfg, func(string) { syncer.Trigger() })
	syncer = server.NewSyncer(cfg, store, proc, events, log)

	// Resume anything interrupted by a restart.
	for _, p := range store.List() {
		if p.Status == server.StatusProcessing {
			proc.Enqueue(p.ID)
		}
	}

	saverDone := make(chan struct{})
	go func() { defer close(saverDone); store.RunSaver(ctx) }()
	proc.Start(ctx)
	go syncer.Run(ctx)
	go server.BackfillDates(ctx, store, log)

	app := &server.App{Cfg: cfg, Log: log, Ring: ring, Store: store, Proc: proc, Sync: syncer, Events: events}
	srv := &http.Server{Addr: cfg.Listen, Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("web UI listening", "addr", cfg.Listen)
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
	<-saverDone
}

func probe(listen string) int {
	addr := listen
	if len(addr) > 0 && addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/api/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
