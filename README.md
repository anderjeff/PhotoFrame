# PhotoFrame

A fast, self-hosted digital photo frame made of two Go programs:

- **Frame** (`cmd/frame`, runs on a Raspberry Pi 3): shows photos from the SD card by drawing directly to the Linux framebuffer. It needs no desktop, X server or browser. Photos arrive already sized for the screen, and the next one is decoded in the background, so changing photos is instant.
- **Server** (`cmd/server`, runs in Docker on the NAS): a web UI for bulk uploads, image processing (EXIF rotation, resizing to the frame's exact resolution, a blurred-photo or solid-color fill around photos that don't match the screen's shape, adjustable in the UI), sync to the frame, and logs from both sides. An optional **On this day** mode (Frame tab) regularly shows photos taken around today's date in earlier years, using the date stored in each photo's EXIF data.

```
 Browser ──upload──▶ Server (NAS, Docker) ──HTTP push──▶ Frame (Pi) ──▶ /dev/fb0
                     originals / renditions              SD card: /var/lib/photoframe/photos
```

## Setting up the Raspberry Pi

1. Flash **Raspberry Pi OS Lite** (32- or 64-bit) and enable SSH. The desktop isn't needed; if you install it, the installer switches the Pi to console boot.
2. On the Pi, run:
   ```sh
   curl -fsSL https://github.com/anderjeff/PhotoFrame/releases/latest/download/install-frame.sh | sudo bash
   ```
3. Note the **FRAME_URL** and **token** that it prints. Reboot once if it asks you to.

To update the Pi, run the same command again. Your config (`/etc/photoframe/frame.env`) and photos (`/var/lib/photoframe/photos`) are kept. To install a specific version: `... | sudo bash -s -- --version v1.2.0`.

During development you can skip releases and push a local build straight to the Pi from Windows:
```powershell
.\scripts\deploy-frame.ps1 -PiHost 192.168.1.50 -User pi           # 32-bit OS
.\scripts\deploy-frame.ps1 -PiHost 192.168.1.50 -User pi -Arch arm64
```

## Server (NAS)

Copy `docker-compose.yml` to the NAS, set `image`, `FRAME_URL` and `FRAME_TOKEN`, then run `docker compose up -d`. Open `http://<nas>:8080`.

All settings are environment variables; see the comments in `docker-compose.yml`. By default the server reads the frame's screen resolution and renders photos to fit it.

## Releasing

One-time setup: in the GitHub repository's **Settings → Secrets and variables → Actions**, add `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` (a Docker Hub access token).

```sh
git tag v0.1.0 && git push origin v0.1.0
```

`.github/workflows/release.yml` then:
- builds `photoframe-linux-armv7` and `photoframe-linux-arm64`, and attaches them, `checksums.txt` and `install-frame.sh` to a GitHub Release;
- pushes `DOCKERHUB_USERNAME/photoframe-server:{version, major.minor, latest}` for amd64 and arm64.

The install script downloads from public GitHub Releases. If the repository is private, the Pi can't download them; use `deploy-frame.ps1` instead.

## Troubleshooting

- **Logs**: the web UI's *Logs* tab shows the server log, the frame's live log, and the frame's systemd journal (which survives restarts and crashes). Set `LOG_LEVEL=debug` on either side for per-photo decode and fade timings.
- **On the Pi**: `journalctl -u photoframe -f`, `systemctl status photoframe`.
- **Frame offline in the UI**: check `FRAME_URL` and that `FRAME_TOKEN` matches on both sides. `curl http://<pi>:8080/api/health` should return `ok`.
- **Adding a big batch from Google Photos**: on photos.google.com (or in the app), select the photos and choose **Download**. Upload the resulting `.zip` as is; the server unpacks it and adds every photo inside. Videos and `.json` files in the zip are skipped. On a phone this avoids the photo picker, which downloads cloud photos one by one and can stall on large selections.
- **HEIC photos** aren't supported. iPhones usually convert to JPEG when uploading through the browser; otherwise set Camera → Formats → Most Compatible.
- You can also copy `.jpg`/`.png` files directly into `/var/lib/photoframe/photos`. They are picked up within a minute, scaled on the Pi, and never touched by sync.
