# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
go test ./...                                   # all tests (includes an in-process end-to-end test)
go test -run TestEndToEnd ./internal/server     # single test
go test -run XXX -bench . ./internal/frame      # pixel conversion / blend benchmarks
go vet ./... && GOOS=linux GOARCH=arm GOARM=7 go vet ./...   # also vet the linux-only framebuffer code
gofmt -l .                                      # CI fails if this prints anything

# Run locally (Windows/macOS: frame runs headless automatically)
go run ./cmd/frame -headless -data ./data/frame -listen :8081 -token dev -log-level debug
DATA_DIR=./data/server FRAME_URL=http://localhost:8081 FRAME_TOKEN=dev go run ./cmd/server   # UI on :8080

# Cross-compile the frame for the Pi
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o dist/photoframe-linux-armv7 ./cmd/frame
```

`-race` needs cgo, so it works in CI (Linux) but not on this Windows machine. Both binaries must stay `CGO_ENABLED=0`: the frame is cross-compiled for ARM and the server image is distroless.

## Architecture

One Go module (`photoframe`) with two programs. They talk only over HTTP. Shared wire types live in `internal/api`.

**Server (`cmd/server`, `internal/server`)**: runs on a NAS in Docker. Configured only by environment variables (`config.go`).
- `http.go`: upload endpoint. It streams multipart parts to `DATA_DIR/tmp` while hashing. The **photo ID is the first 16 bytes of the original's SHA-256**, so duplicate uploads are no-ops. Also serves the embedded web UI (`web/`, vanilla JS, no build step) and proxies `/api/frame/*` to the frame, so the browser never talks to the Pi directly.
- `processor.go` + `imaging.go`: worker pool that renders each original into an image of **exactly the frame's resolution** (EXIF orientation, CatmullRom scaling, background fill from `background.go`). It scales before rotating to avoid full-resolution rotation work. Output goes to `display/<id>.jpg` and `thumbs/<id>.jpg`.
- `sync.go`: `Syncer` reconciles the server's photo set with the frame's manifest. It runs on a timer and immediately (debounced) when triggered by processing or deletes. It pushes newest uploads first and deletes photos the server no longer has.
- `store.go`: in-memory index persisted to `DATA_DIR/index.json` (debounced). `events.go` broadcasts changes to the UI over SSE.

**Frame (`cmd/frame`, `internal/frame`)**: runs on a Raspberry Pi 3 as a systemd service (installed by `scripts/install-frame.sh`).
- `display_linux.go`: mmaps `/dev/fb0` via ioctls and switches `/dev/tty1` to `KD_GRAPHICS`. If the framebuffer can't be opened, or on non-Linux systems, it falls back to `Headless`, so the API and logs stay reachable.
- `pixels.go`: `Frame` holds pixels in the **display's native format**, so showing one is a memcpy. JPEG `YCbCr` is converted straight to that format, split across all cores. Fades blend native buffers.
- `slideshow.go`: a single `Run` goroutine owns all playlist state (`fresh` queue for new arrivals → shuffled `queue` → `history`). Everything else sends it `event`s. It keeps one background prefetch at a time, so the next photo is ready before it's shown.
- `store.go`: files in `DATA_DIR/photos`. Server-managed files are named `<id>_<rev>.jpg`. Other `.jpg`/`.png` files placed by hand are shown but never synced or deleted. Writes are atomic (temp file → fsync → rename).
- `httpapi.go`: token-protected API (`X-PhotoFrame-Token`) used by the server: manifest, PUT/DELETE photos, settings, control, logs, journal, screenshot.

**Renditions and `rev`**: a rendition's rev is `<W>x<H>v<procVersion><bgTag>` (e.g. `1920x1080v1`, or `1920x1080v1c000000` for a black background; the default blur has no tag). When the frame reports a new resolution, the background is changed in the UI (saved in `index.json`), or `procVersion` in `processor.go` is bumped, every photo is re-rendered and re-pushed automatically. A background-only change keeps thumbnails, and the frame redraws the on-screen photo when its file is replaced. **Bump `procVersion` whenever rendering output changes.**

**Logging**: `internal/logx` sends slog output both to stdout (journald / docker logs) and to an in-memory ring served at `/api/logs?since=N&level=`. The UI polls that endpoint incrementally. Log the timings that matter for performance work (decode and total durations are already logged).

## Release / deploy

- Pushing a `v*` tag runs `.github/workflows/release.yml`: it builds the Pi binaries and attaches them, `checksums.txt` and `install-frame.sh` to a GitHub Release, then pushes the multi-arch server image to Docker Hub (needs secrets `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN`).
- The Pi updates by re-running `install-frame.sh`, which is idempotent and keeps `/etc/photoframe/frame.env`. `scripts/deploy-frame.ps1` builds locally and installs over SSH for development.
- `install-frame.sh` defaults to the `anderjeff/PhotoFrame` repo; the release workflow rewrites that to `github.repository` in the attached copy, so forks work. `docker-compose.yml` has a `YOUR_DOCKERHUB_USER` placeholder.
- Shell scripts must keep LF line endings (enforced by `.gitattributes`).
