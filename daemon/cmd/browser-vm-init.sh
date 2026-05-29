#!/bin/sh
# browser-vm-init.sh — PID-1 init for the browser-tier Firecracker VM.
#
# Responsibilities:
#   1. Mount /proc (required by Chrome and socat; may already be mounted by the
#      kernel if the rootfs image includes a populated /etc/fstab, but mounting
#      again is harmless when it is already up).
#   2. Write a pinchtab config JSON to a temp path from env-provided values.
#   3. Start pinchtab server in the background (must be up before socat bridges
#      traffic into it).
#   4. exec socat as the long-lived foreground process so it becomes the
#      process that the Firecracker host monitors via SIGCHLD / wait.
#
# Environment variables (all optional; shown with defaults):
#   PINCHTAB_TOKEN          — server auth token; empty = no auth (dev only)
#   IX_BROWSER_STATE_DIR    — host-visible state dir; default /var/lib/ix/browser-state
#
# The config file path is written to /run/pinchtab-server.json and exported as
# PINCHTAB_CONFIG so pinchtab picks it up on startup.

set -e

# ---------------------------------------------------------------------------
# 1. Minimal init hygiene for PID 1 in a Firecracker guest
# ---------------------------------------------------------------------------
mount -t proc proc /proc 2>/dev/null || true
mount -t sysfs sysfs /sys 2>/dev/null || true
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts 2>/dev/null || true

# ---------------------------------------------------------------------------
# 2. Write pinchtab server config JSON
# ---------------------------------------------------------------------------
STATE_DIR="${IX_BROWSER_STATE_DIR:-/var/lib/ix/browser-state}"
mkdir -p "$STATE_DIR"

CONFIG_PATH=/run/pinchtab-server.json

# server.token: if PINCHTAB_TOKEN is set it overrides the file value, but
# ensureMandatoryToken() in cmd_config_load.go will error when PINCHTAB_CONFIG
# is set and the file exists but has no token AND PINCHTAB_TOKEN is also empty.
# Writing the token into the file (even if empty string) satisfies the check
# for the empty-token dev case because fc.Server.Token == "" causes the code to
# fall through to EnsureFileToken (auto-generate), which is fine.
# When PINCHTAB_TOKEN is non-empty it overrides via env and this value is ignored.
TOKEN_VALUE="${PINCHTAB_TOKEN:-}"

cat > "$CONFIG_PATH" <<EOF
{
  "server": {
    "port": "9867",
    "bind": "127.0.0.1",
    "token": "${TOKEN_VALUE}"
  },
  "profiles": {
    "baseDir": "${STATE_DIR}"
  },
  "instanceDefaults": {
    "mode": "headless"
  },
  "security": {
    "allowEvaluate": true
  },
  "multiInstance": {
    "instancePortStart": 9868,
    "instancePortEnd": 9968
  }
}
EOF

export PINCHTAB_CONFIG="$CONFIG_PATH"

# ---------------------------------------------------------------------------
# 3. Start pinchtab server in the background
#    Must be started BEFORE socat so that by the time the first vsock
#    connection arrives, pinchtab is already listening on 127.0.0.1:9867.
# ---------------------------------------------------------------------------
pinchtab server &
PINCHTAB_PID=$!

# Wait for pinchtab to bind its port (poll /health, up to 30 s).
DEADLINE=30
i=0
while [ $i -lt $DEADLINE ]; do
    if wget -q -O /dev/null http://127.0.0.1:9867/health 2>/dev/null; then
        break
    fi
    sleep 1
    i=$((i + 1))
done

if [ $i -eq $DEADLINE ]; then
    echo "browser-vm-init: pinchtab did not become healthy within ${DEADLINE}s; aborting" >&2
    kill "$PINCHTAB_PID" 2>/dev/null || true
    exit 1
fi

echo "browser-vm-init: pinchtab healthy on 127.0.0.1:9867 (pid $PINCHTAB_PID)" >&2

# ---------------------------------------------------------------------------
# 4. exec socat as the long-lived foreground process (new PID 1 after exec)
#    Bridges AF_VSOCK port 1024 (Firecracker host→guest delivery port) to
#    TCP 127.0.0.1:9867 (pinchtab server).
#    fork: each vsock connection gets its own TCP connection to pinchtab.
# ---------------------------------------------------------------------------
exec socat VSOCK-LISTEN:1024,reuseaddr,fork TCP:127.0.0.1:9867
