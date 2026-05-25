#!/bin/bash
# ix-init: PID 1 init script for Firecracker MicroVM
# Mounts filesystems, sets up networking, parses kernel cmdline, starts daemon

echo "ix-init: Starting..."

# Mount essential filesystems (skip if already mounted by kernel)
mountpoint -q /proc  || mount -t proc proc /proc
mountpoint -q /sys   || mount -t sysfs sysfs /sys
mountpoint -q /dev   || mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts /dev/shm
mountpoint -q /dev/pts || mount -t devpts devpts /dev/pts
mountpoint -q /dev/shm || mount -t tmpfs tmpfs /dev/shm
mountpoint -q /tmp     || mount -t tmpfs tmpfs /tmp
mountpoint -q /run     || mount -t tmpfs tmpfs /run

# Set hostname
echo "ix-sandbox" > /etc/hostname
hostname -F /etc/hostname

# Parse kernel command line for ix.env.* variables and export them
if [ -f /proc/cmdline ]; then
  for param in $(cat /proc/cmdline); do
    if [[ "$param" == ix.env.* ]]; then
      # Strip "ix.env." prefix and export
      export "${param#ix.env.}"
    fi
  done
fi

# Set default DNS
echo "nameserver 8.8.8.8" > /etc/resolv.conf

# Create workspace directory
mkdir -p /workspace

# Export vsock port configuration
export IX_VSOCK_PORT=1024
export IX_VSOCK_READY_PORT=1025

# Signal handling: forward SIGTERM and SIGINT to daemon
trap_handler() {
  if [ -n "$DAEMON_PID" ] && kill -0 "$DAEMON_PID" 2>/dev/null; then
    kill "$DAEMON_PID"
    wait "$DAEMON_PID" 2>/dev/null || true
  fi
  exit 0
}
trap trap_handler SIGTERM SIGINT

# Start daemon in background
/usr/local/bin/ixd &
DAEMON_PID=$!

# Wait for daemon process
wait "$DAEMON_PID"
EXIT_CODE=$?

# Exit with daemon's exit code
exit "$EXIT_CODE"
