package frame

import (
	"context"
	"encoding/json"
	"errors"
	"image/jpeg"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"photoframe/internal/api"
	"photoframe/internal/logx"
	"photoframe/internal/version"
)

const maxPhotoBytes = 64 << 20

// API is the frame's HTTP interface, used by the server for sync, control
// and troubleshooting.
type API struct {
	Log     *slog.Logger
	Ring    *logx.Ring
	Store   *Store
	Show    *Slideshow
	Display Display
	DataDir string
	Token   string
	Started time.Time
}

func (a *API) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/info", a.info)
	m.HandleFunc("GET /api/photos", func(w http.ResponseWriter, r *http.Request) {
		api.WriteJSON(w, http.StatusOK, a.Store.Manifest())
	})
	m.HandleFunc("GET /api/photos/{key}", a.getPhoto)
	m.HandleFunc("PUT /api/photos/{id}", a.putPhoto)
	m.HandleFunc("DELETE /api/photos/{id}", a.deletePhoto)
	m.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		api.WriteJSON(w, http.StatusOK, a.Show.Settings())
	})
	m.HandleFunc("PUT /api/settings", a.putSettings)
	m.HandleFunc("PUT /api/featured", func(w http.ResponseWriter, r *http.Request) {
		var f api.Featured
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&f); err != nil {
			api.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		a.Show.SetFeatured(f)
		w.WriteHeader(http.StatusNoContent)
	})
	m.HandleFunc("POST /api/control/{action}", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Show.Control(r.PathValue("action")); err != nil {
			api.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.Log.Info("control", "action", r.PathValue("action"))
		w.WriteHeader(http.StatusNoContent)
	})
	m.Handle("GET /api/logs", a.Ring)
	m.HandleFunc("GET /api/logs/journal", a.journal)
	m.HandleFunc("GET /api/screen", a.screen)

	root := http.NewServeMux()
	root.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	root.Handle("/api/", api.RequireToken(a.Token, m))
	return logx.Middleware(a.Log, root)
}

func (a *API) info(w http.ResponseWriter, r *http.Request) {
	wd, ht := a.Display.Size()
	cur, paused := a.Show.Status()
	host, _ := os.Hostname()
	free, total := diskUsage(a.DataDir)
	api.WriteJSON(w, http.StatusOK, api.FrameInfo{
		Version: version.Version, Hostname: host, Addresses: LocalAddresses(),
		Display: a.Display.Name(), Width: wd, Height: ht,
		PhotoCount: a.Store.Count(), Current: cur, Paused: paused,
		UptimeSeconds: int64(time.Since(a.Started).Seconds()),
		CPUTempC:      cpuTemp(), DiskFreeBytes: free, DiskTotalBytes: total, MemAvailBytes: memAvailable(),
		Settings: a.Show.Settings(), FeaturedKey: a.Show.FeaturedKey(),
	})
}

func (a *API) getPhoto(w http.ResponseWriter, r *http.Request) {
	p, ok := a.Store.Get(r.PathValue("key"))
	if !ok {
		api.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	http.ServeFile(w, r, p.Path)
}

func (a *API) putPhoto(w http.ResponseWriter, r *http.Request) {
	id, rev := r.PathValue("id"), r.URL.Query().Get("rev")
	if !api.ValidID(id) || !api.ValidRev(rev) {
		api.WriteError(w, http.StatusBadRequest, "invalid id or rev")
		return
	}
	start := time.Now()
	if err := a.Store.Put(id, rev, r.Body, maxPhotoBytes); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrNotJPEG) {
			status = http.StatusBadRequest
		}
		a.Log.Error("store photo failed", "id", id, "err", err)
		api.WriteError(w, status, err.Error())
		return
	}
	a.Log.Info("photo received", "id", id, "rev", rev, "dur", time.Since(start).Round(time.Millisecond))
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) deletePhoto(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !api.ValidID(id) {
		api.WriteError(w, http.StatusBadRequest, "invalid id")
		return
	}
	found, err := a.Store.Delete(id)
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if found {
		a.Log.Info("photo deleted", "id", id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) putSettings(w http.ResponseWriter, r *http.Request) {
	var s api.Settings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&s); err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	s, err := a.Show.SetSettings(s)
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	api.WriteJSON(w, http.StatusOK, s)
}

// journal returns the systemd journal for the service: survives restarts and
// includes crash output that the in-memory ring would lose.
func (a *API) journal(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.URL.Query().Get("lines"))
	if err != nil || n <= 0 || n > 10000 {
		n = 500
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "journalctl", "-u", "photoframe", "-n", strconv.Itoa(n), "--no-pager", "-o", "short-iso").CombinedOutput()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("journalctl failed: " + err.Error() + "\n"))
	}
	w.Write(out)
}

func (a *API) screen(w http.ResponseWriter, r *http.Request) {
	img := a.Show.Screenshot()
	if img == nil {
		api.WriteError(w, http.StatusNotFound, "nothing on screen yet")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	_ = jpeg.Encode(w, img, &jpeg.Options{Quality: 75})
}

// LocalAddresses lists the frame's non-loopback IPv4 addresses.
func LocalAddresses() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

func cpuTemp() float64 {
	b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp")
	if err != nil {
		return 0
	}
	v, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return float64(v) / 1000
}

func memAvailable() uint64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemAvailable:"); ok {
			kb, _ := strconv.ParseUint(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB")), 10, 64)
			return kb * 1024
		}
	}
	return 0
}
