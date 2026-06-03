#!/bin/sh
# browser-vm-init.sh — PID-1 init for the browser-tier Firecracker VM.
#
# Responsibilities:
#   1. Mount /proc, /sys, /dev, /dev/pts and /dev/shm (Chrome renderers crash
#      without a writable /dev/shm).
#   2. Parse ix.env.* kernel cmdline params into the environment (mirrors the
#      base tier's ix-init, which is how the host passes PINCHTAB_TOKEN etc.).
#   3. Write a pinchtab config JSON from env-provided values.
#   4. Start the pinchtab *bridge* in the background (must be up before socat
#      bridges traffic into it). `pinchtab server` runs an interactive first-run
#      security wizard that blocks forever in a headless VM — `bridge` is the
#      programmatic, non-interactive entrypoint the daemon uses in local mode.
#   5. exec socat as the long-lived foreground process so it becomes the
#      process that the Firecracker host monitors via SIGCHLD / wait.
#
# Environment variables (all optional; shown with defaults):
#   PINCHTAB_TOKEN          — server auth token; default "ix-internal" (shared
#                             with the host Gateway). Passed as
#                             ix.env.PINCHTAB_TOKEN on the kernel cmdline.
#   IX_BROWSER_STATE_DIR    — profile state dir; default /var/lib/ix/browser-state
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
# Chrome's renderer/GPU processes use POSIX shared memory; without a writable
# /dev/shm they crash on launch. devtmpfs does not provide it, so mount a tmpfs.
mkdir -p /dev/shm
mount -t tmpfs -o size=1g tmpfs /dev/shm 2>/dev/null || true
mount -t tmpfs tmpfs /tmp 2>/dev/null || true
mount -t tmpfs tmpfs /run 2>/dev/null || true

# Bring up the loopback interface. Firecracker guests start with `lo` DOWN;
# pinchtab binds 127.0.0.1:9867 and the socat bridge connects to it, both of
# which fail (no route to 127.0.0.1) until loopback is up.
ip link set lo up 2>/dev/null || true

# Seed DNS for Chrome. The kernel ip= arg populates /proc/net/pnp, but Chrome
# resolves via /etc/resolv.conf, so write it explicitly (belt-and-suspenders).
echo "nameserver 8.8.8.8" > /etc/resolv.conf 2>/dev/null || true

# ---------------------------------------------------------------------------
# 1b. Parse kernel cmdline ix.env.* params into the environment.
#     Firecracker passes host env as `ix.env.KEY=VALUE` on the cmdline; the
#     base tier's ix-init does the same. Without this, PINCHTAB_TOKEN injected
#     by the host would never reach pinchtab.
# ---------------------------------------------------------------------------
if [ -r /proc/cmdline ]; then
    for param in $(cat /proc/cmdline); do
        case "$param" in
            ix.env.*) export "${param#ix.env.}" ;;
        esac
    done
fi

# ---------------------------------------------------------------------------
# 2. Write pinchtab server config JSON
# ---------------------------------------------------------------------------
STATE_DIR="${IX_BROWSER_STATE_DIR:-/var/lib/ix/browser-state}"
mkdir -p "$STATE_DIR"

CONFIG_PATH=/run/pinchtab-server.json

# pinchtab requires a non-empty server token when PINCHTAB_CONFIG is set. Default
# to the same fixed internal token the local-mode daemon uses so the host
# Gateway (which sends Bearer ix-internal by default) and the guest agree with
# zero operator config. Operators override via PINCHTAB_TOKEN.
TOKEN_VALUE="${PINCHTAB_TOKEN:-ix-internal}"

cat > "$CONFIG_PATH" <<EOF
{
  "configVersion": "0.8.0",
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
    "idpi": { "enabled": false },
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
# 3. Start the pinchtab orchestrator (server) in the background
#    Must be started BEFORE socat so that by the time the first vsock
#    connection arrives, pinchtab is already listening on 127.0.0.1:9867.
#    Use `server` (the multi-instance orchestrator serving /instances/*), which
#    the host Gateway requires — NOT `bridge` (single-instance, no /instances
#    API → gateway gets 404 on /instances/start). `server` normally runs an
#    interactive first-run security wizard, but it is skipped here because the
#    config above carries a current "configVersion" (pinchtab's NeedsWizard
#    returns false), so it starts non-interactively in a headless VM.
# ---------------------------------------------------------------------------
pinchtab server &
PINCHTAB_PID=$!

# Wait for pinchtab to bind its port (poll /health, up to 30 s).
DEADLINE=30
i=0
while [ $i -lt $DEADLINE ]; do
    # pinchtab's /health (like all its endpoints) requires the Bearer token.
    if wget -q -O /dev/null --header="Authorization: Bearer ${TOKEN_VALUE}" \
        http://127.0.0.1:9867/health 2>/dev/null; then
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
