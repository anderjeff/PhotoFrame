package server

import (
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"photoframe/internal/logx"
)

// Config is read from environment variables (see docker-compose.yml).
type Config struct {
	Listen       string        // LISTEN
	DataDir      string        // DATA_DIR
	FrameURL     string        // FRAME_URL, e.g. http://192.168.1.50:8080
	FrameToken   string        // FRAME_TOKEN (must match the frame's)
	Width        int           // FRAME_WIDTH; 0 = use the frame's reported resolution
	Height       int           // FRAME_HEIGHT
	Workers      int           // WORKERS: concurrent image-processing jobs
	JPEGQuality  int           // JPEG_QUALITY for display renditions
	Background   string        // BACKGROUND: "blur" or "black" fill around non-matching aspect ratios
	SyncInterval time.Duration // SYNC_INTERVAL: periodic reconcile with the frame
	MaxUploadMB  int64         // MAX_UPLOAD_MB per file
	LogLevel     slog.Level    // LOG_LEVEL
}

func ConfigFromEnv() Config {
	str := func(k, def string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		return def
	}
	num := func(k string, def int) int {
		if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
			return v
		}
		return def
	}
	interval, err := time.ParseDuration(str("SYNC_INTERVAL", "15s"))
	if err != nil || interval < time.Second {
		interval = 15 * time.Second
	}
	c := Config{
		Listen:       str("LISTEN", ":8080"),
		DataDir:      str("DATA_DIR", "/data"),
		FrameURL:     strings.TrimRight(str("FRAME_URL", "http://photoframe.local:8080"), "/"),
		FrameToken:   os.Getenv("FRAME_TOKEN"),
		Width:        num("FRAME_WIDTH", 0),
		Height:       num("FRAME_HEIGHT", 0),
		Workers:      num("WORKERS", min(runtime.NumCPU(), 4)),
		JPEGQuality:  min(max(num("JPEG_QUALITY", 90), 50), 100),
		Background:   str("BACKGROUND", "blur"),
		SyncInterval: interval,
		MaxUploadMB:  int64(num("MAX_UPLOAD_MB", 200)),
		LogLevel:     logx.ParseLevel(os.Getenv("LOG_LEVEL"), slog.LevelInfo),
	}
	c.Workers = max(c.Workers, 1)
	return c
}
