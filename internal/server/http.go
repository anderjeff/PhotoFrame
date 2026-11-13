package server

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"photoframe/internal/api"
	"photoframe/internal/logx"
	"photoframe/internal/version"
)

//go:embed web
var webFS embed.FS

// App wires the server's components to HTTP.
type App struct {
	Cfg    Config
	Log    *slog.Logger
	Ring   *logx.Ring
	Store  *Store
	Proc   *Processor
	Sync   *Syncer
	Events *Broker
}

func (a *App) Handler() http.Handler {
	m := http.NewServeMux()
	web, _ := fs.Sub(webFS, "web")
	m.Handle("GET /", http.FileServerFS(web))

	m.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	m.HandleFunc("GET /api/status", a.status)
	m.Handle("GET /api/events", a.Events)
	m.Handle("GET /api/logs", a.Ring)
	m.HandleFunc("POST /api/sync", func(w http.ResponseWriter, r *http.Request) {
		a.Sync.Trigger()
		w.WriteHeader(http.StatusAccepted)
	})

	m.HandleFunc("POST /api/upload", a.upload)
	m.HandleFunc("GET /api/photos", func(w http.ResponseWriter, r *http.Request) {
		api.WriteJSON(w, http.StatusOK, a.Store.List())
	})
	m.HandleFunc("DELETE /api/photos/{id}", a.deleteOne)
	m.HandleFunc("POST /api/photos/delete", a.deleteMany)
	m.HandleFunc("POST /api/photos/{id}/retry", a.retry)
	m.HandleFunc("GET /api/photos/{id}/{kind}", a.photoFile)

	m.HandleFunc("GET /api/background", a.getBackground)
	m.HandleFunc("PUT /api/background", a.putBackground)
	m.HandleFunc("GET /api/background/preview", a.previewBackground)

	// Frame passthroughs, so the browser only ever talks to the server.
	m.HandleFunc("GET /api/frame/info", a.proxy("/api/info"))
	m.HandleFunc("GET /api/frame/logs", a.proxy("/api/logs"))
	m.HandleFunc("GET /api/frame/journal", a.proxy("/api/logs/journal"))
	m.HandleFunc("GET /api/frame/screen", a.proxy("/api/screen"))
	m.HandleFunc("GET /api/frame/settings", a.proxy("/api/settings"))
	m.HandleFunc("PUT /api/frame/settings", a.proxy("/api/settings"))
	m.HandleFunc("POST /api/frame/control/{action}", func(w http.ResponseWriter, r *http.Request) {
		a.proxy("/api/control/"+r.PathValue("action"))(w, r)
	})
	return logx.Middleware(a.Log, m)
}

func (a *App) status(w http.ResponseWriter, r *http.Request) {
	counts := map[string]int{}
	for _, p := range a.Store.List() {
		counts[p.Status]++
		counts["total"]++
		if p.Synced {
			counts["synced"]++
		}
	}
	dw, dh := a.Store.DisplaySize()
	api.WriteJSON(w, http.StatusOK, map[string]any{
		"version": version.Version, "frame": a.Sync.Status(), "counts": counts,
		"display": map[string]int{"width": dw, "height": dh}, "queue": a.Proc.Pending(),
		"maxUploadMB": a.Cfg.MaxUploadMB,
	})
}

type uploadResult struct {
	Name      string `json:"name"`
	ID        string `json:"id,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`
	Error     string `json:"error,omitempty"`
	Photo     *Photo `json:"photo,omitempty"`
}

var allowedTypes = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp", "image/bmp": ".bmp",
}

// upload streams multipart files straight to disk while hashing them, so even
// very large batches never sit in memory. The browser sends one file per
// request, several requests in parallel.
func (a *App) upload(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	var results []uploadResult
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			api.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if part.FileName() == "" {
			part.Close()
			continue
		}
		results = append(results, a.saveUpload(part))
		part.Close()
	}
	status := http.StatusOK
	if len(results) == 1 && results[0].Error != "" {
		status = http.StatusUnprocessableEntity
	}
	api.WriteJSON(w, status, results)
}

func (a *App) saveUpload(part *multipart.Part) uploadResult {
	name := filepath.Base(part.FileName())
	res := uploadResult{Name: name}
	head := make([]byte, 512)
	n, err := io.ReadFull(part, head)
	if errors.Is(err, io.EOF) {
		res.Error = "empty file"
		return res
	}
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		res.Error = "read failed: " + err.Error()
		return res
	}
	head = head[:n]
	ext, ok := allowedTypes[http.DetectContentType(head)]
	if !ok {
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, ".heic") || strings.HasSuffix(lower, ".heif") {
			res.Error = "HEIC is not supported; export as JPEG (on iPhone: Settings → Camera → Formats → Most Compatible)"
		} else {
			res.Error = "not a supported image (JPEG, PNG, GIF, WebP, BMP)"
		}
		a.Log.Warn("upload rejected", "name", name, "reason", res.Error)
		return res
	}

	tmp, err := os.CreateTemp(a.Store.TmpDir(), "up-*")
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	mw := io.MultiWriter(tmp, h)
	mw.Write(head)
	limit := a.Cfg.MaxUploadMB << 20
	copied, err := io.Copy(mw, io.LimitReader(part, limit))
	size := copied + int64(len(head))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		res.Error = "upload failed: " + err.Error()
		a.Log.Warn("upload failed", "name", name, "err", err)
		return res
	}
	if size > limit {
		res.Error = "file too large"
		return res
	}

	id := hex.EncodeToString(h.Sum(nil)[:16])
	res.ID = id
	if existing, ok := a.Store.Get(id); ok {
		res.Duplicate, res.Photo = true, &existing
		a.Log.Info("duplicate upload skipped", "name", name, "id", id)
		return res
	}
	p := Photo{ID: id, Name: name, Ext: ext, Size: size, UploadedAt: time.Now(), Status: StatusProcessing}
	if err := os.Rename(tmp.Name(), a.Store.OriginalPath(p)); err != nil {
		res.Error = err.Error()
		return res
	}
	if !a.Store.Add(p) {
		res.Duplicate = true // a concurrent upload of the same file won the race
		return res
	}
	res.Photo = &p
	a.Proc.Enqueue(id)
	a.Log.Info("photo uploaded", "name", name, "id", id, "bytes", size)
	return res
}

func (a *App) deleteOne(w http.ResponseWriter, r *http.Request) {
	if !a.Store.Delete(r.PathValue("id")) {
		api.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	a.Log.Info("photo deleted", "id", r.PathValue("id"))
	a.Sync.Trigger()
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) deleteMany(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		api.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	n := 0
	for _, id := range req.IDs {
		if a.Store.Delete(id) {
			n++
		}
	}
	a.Log.Info("photos deleted", "count", n)
	a.Sync.Trigger()
	api.WriteJSON(w, http.StatusOK, map[string]int{"deleted": n})
}

func (a *App) retry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.Store.Update(id, func(p *Photo) { p.Status, p.Error = StatusProcessing, "" }); !ok {
		api.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	a.Proc.Enqueue(id)
	w.WriteHeader(http.StatusAccepted)
}

func (a *App) photoFile(w http.ResponseWriter, r *http.Request) {
	p, ok := a.Store.Get(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	var path string
	switch r.PathValue("kind") {
	case "thumb":
		path = a.Store.ThumbPath(p.ID)
	case "display":
		path = a.Store.DisplayPath(p.ID)
	case "original":
		path = a.Store.OriginalPath(p)
		w.Header().Set("Content-Disposition", `inline; filename="`+strings.ReplaceAll(p.Name, `"`, "")+`"`)
	default:
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Has("v") { // URL includes the rev, so it can be cached forever
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeFile(w, r, path)
}

// proxy forwards the request to the frame and streams the response back.
func (a *App) proxy(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target := path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		var body io.Reader
		if r.Body != nil && r.Method != http.MethodGet {
			body = r.Body
		}
		resp, err := a.Sync.Client().Do(r.Context(), r.Method, target, body, r.ContentLength)
		if err != nil {
			api.WriteError(w, http.StatusBadGateway, "frame unreachable: "+err.Error())
			return
		}
		defer resp.Body.Close()
		for _, h := range []string{"Content-Type", "Content-Length", "Cache-Control"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		if r.Method != http.MethodGet && resp.StatusCode < 300 {
			a.Sync.Trigger() // refresh frame status (e.g. paused, settings) promptly
		}
	}
}
