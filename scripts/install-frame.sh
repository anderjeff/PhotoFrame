#!/usr/bin/env bash
# Installs or updates the PhotoFrame display service on a Raspberry Pi.
# Re-run it any time to update to the latest release; config and photos are kept.
#
#   curl -fsSL https://github.com/anderjeff/PhotoFrame/releases/latest/download/install-frame.sh | sudo bash
#
# Options (after `sudo bash -s --` when piping):
#   --version vX.Y.Z   install a specific release instead of the latest
#   --token TOKEN      set FRAME_TOKEN (otherwise one is generated on first install)
#   --binary PATH      install a locally built binary instead of downloading
#   --keep-desktop     don't switch the Pi to console boot (the desktop draws over the photos)
set -euo pipefail

REPO="${PHOTOFRAME_REPO:-anderjeff/PhotoFrame}"
VERSION="latest"
TOKEN=""
BINARY=""
KEEP_DESKTOP=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --binary) BINARY="$2"; shift 2 ;;
    --keep-desktop) KEEP_DESKTOP=1; shift ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

[[ $EUID -eq 0 ]] || { echo "run as root (sudo)" >&2; exit 1; }

BIN=/usr/local/bin/photoframe
CONF_DIR=/etc/photoframe
CONF=$CONF_DIR/frame.env
DATA=/var/lib/photoframe
UNIT=/etc/systemd/system/photoframe.service

log() { echo -e "\033[1;32m==>\033[0m $*"; }

# --- binary -----------------------------------------------------------------
if [[ -n "$BINARY" ]]; then
  log "Installing local binary $BINARY"
  install -m 0755 "$BINARY" "$BIN.new"
else
  case "$(dpkg --print-architecture 2>/dev/null || uname -m)" in
    armhf|armv7l|armv6l) ARCH=armv7 ;;
    arm64|aarch64) ARCH=arm64 ;;
    *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
  esac
  if [[ "$VERSION" == "latest" ]]; then
    BASE="https://github.com/$REPO/releases/latest/download"
  else
    BASE="https://github.com/$REPO/releases/download/$VERSION"
  fi
  TMP=$(mktemp -d)
  trap 'rm -rf "$TMP"' EXIT
  log "Downloading photoframe ($VERSION, $ARCH) from $REPO"
  curl -fsSL -o "$TMP/photoframe-linux-$ARCH" "$BASE/photoframe-linux-$ARCH"
  curl -fsSL -o "$TMP/checksums.txt" "$BASE/checksums.txt"
  (cd "$TMP" && grep " photoframe-linux-$ARCH\$" checksums.txt | sha256sum -c --quiet -)
  install -m 0755 "$TMP/photoframe-linux-$ARCH" "$BIN.new"
fi
mv -f "$BIN.new" "$BIN" # atomic replace, safe while the old version is running
log "Installed $("$BIN" -version)"

# --- config -----------------------------------------------------------------
mkdir -p "$CONF_DIR" "$DATA/photos"
if [[ ! -f "$CONF" ]]; then
  # Fixed-size read: `tr </dev/urandom | head` gets SIGPIPE and aborts under pipefail.
  [[ -n "$TOKEN" ]] || TOKEN=$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')
  cat >"$CONF" <<EOF
# PhotoFrame frame settings. Restart after editing: sudo systemctl restart photoframe
LISTEN=:8080
DATA_DIR=$DATA
# Must match FRAME_TOKEN on the server.
FRAME_TOKEN=$TOKEN
LOG_LEVEL=info
FB_DEVICE=/dev/fb0
TTY=/dev/tty1
EOF
  chmod 600 "$CONF"
  log "Created $CONF"
elif [[ -n "$TOKEN" ]]; then
  sed -i "s/^FRAME_TOKEN=.*/FRAME_TOKEN=$TOKEN/" "$CONF"
  log "Updated FRAME_TOKEN in $CONF"
fi

# --- systemd ----------------------------------------------------------------
cat >"$UNIT" <<'EOF'
[Unit]
Description=PhotoFrame display
After=network-online.target
Wants=network-online.target
# The frame owns tty1; stop the login prompt from drawing over the photos.
Conflicts=getty@tty1.service
After=getty@tty1.service

[Service]
EnvironmentFile=/etc/photoframe/frame.env
ExecStart=/usr/local/bin/photoframe
Restart=always
RestartSec=2
Nice=-5

[Install]
WantedBy=multi-user.target
EOF

# --- boot settings (first install only; need a reboot) ---------------------
REBOOT=0
for CMDLINE in /boot/firmware/cmdline.txt /boot/cmdline.txt; do
  [[ -f "$CMDLINE" ]] || continue
  for opt in consoleblank=0 vt.global_cursor_default=0; do
    if ! grep -qw "$opt" "$CMDLINE"; then
      sed -i "1 s/\$/ $opt/" "$CMDLINE"
      REBOOT=1
    fi
  done
  break
done
if [[ $KEEP_DESKTOP -eq 0 && "$(systemctl get-default)" == "graphical.target" ]]; then
  log "Switching to console boot (the desktop would cover the photos); use --keep-desktop to skip"
  systemctl set-default multi-user.target
  REBOOT=1
fi

systemctl daemon-reload
systemctl enable photoframe.service >/dev/null 2>&1
systemctl restart photoframe.service
sleep 1
systemctl --no-pager --lines=0 status photoframe.service || true

IP=$(hostname -I 2>/dev/null | awk '{print $1}')
echo
log "PhotoFrame is running."
echo "    Frame URL for the server:  FRAME_URL=http://${IP:-<pi-ip>}:8080"
echo "    Token for the server:      $(grep '^FRAME_TOKEN=' "$CONF" | cut -d= -f2-)"
echo "    Logs:                      journalctl -u photoframe -f"
if [[ $REBOOT -eq 1 ]]; then
  echo
  log "Boot settings changed: reboot once to apply (sudo reboot)."
fi
