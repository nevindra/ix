#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TIER="${1:-base}"
IX_ROOTFS_SIZE="${IX_ROOTFS_SIZE:-2048}"
IX_ROOTFS_IMAGE="${IX_ROOTFS_IMAGE:-/opt/ix/rootfs/${TIER}.ext4}"
TEMP_ROOTFS=""

readonly VALID_TIERS=("base" "browser" "browser-vm")
readonly IMAGE_TAG="ix:${TIER}"
readonly DAEMON_BIN="../daemon/target/x86_64-unknown-linux-musl/release/ixd"

cleanup() {
  if [[ -n "$TEMP_ROOTFS" && -d "$TEMP_ROOTFS" ]]; then
    # Unmount if mounted
    if mountpoint -q "$TEMP_ROOTFS" 2>/dev/null; then
      sudo umount -l "$TEMP_ROOTFS" || true
    fi
    sudo rm -rf "$TEMP_ROOTFS"
  fi
}

trap cleanup EXIT

validate_tier() {
  local tier="$1"
  for valid in "${VALID_TIERS[@]}"; do
    if [[ "$tier" == "$valid" ]]; then
      return 0
    fi
  done
  echo "Error: Invalid tier '$tier'. Valid tiers: ${VALID_TIERS[*]}" >&2
  return 1
}

ensure_output_dir() {
  local dir
  dir="$(dirname "$1")"

  if [[ ! -d "$dir" ]]; then
    mkdir -p "$dir" || sudo mkdir -p "$dir"
  fi

  if [[ ! -w "$dir" ]]; then
    sudo chown "$(id -u):$(id -g)" "$dir" || {
      echo "Warning: Cannot write to $dir, will use sudo for image creation" >&2
      return 1
    }
  fi
  return 0
}

create_container_export() {
  local image_tag="$1"
  local temp_dir="$2"

  echo "Creating temporary container from ${image_tag}..."

  local container_id
  container_id=$(docker create "$image_tag")
  trap "docker rm -f '$container_id' >/dev/null 2>&1 || true; cleanup" EXIT

  echo "Exporting container filesystem..."
  docker export "$container_id" | tar -xf - -C "$temp_dir"

  echo "✓ Container exported to temporary directory"
}

copy_daemon_binary() {
  local temp_dir="$1"

  if [[ -f "$DAEMON_BIN" ]]; then
    echo "Copying ixd daemon binary..."
    sudo install -m 755 "$DAEMON_BIN" "${temp_dir}/usr/local/bin/ixd"
    echo "✓ Daemon binary installed"
  else
    echo "Warning: Daemon binary not found at $DAEMON_BIN" >&2
    echo "         Build the daemon first with: cd ../daemon && cargo build --release --target x86_64-unknown-linux-musl" >&2
  fi
}

create_init_script() {
  local temp_dir="$1"

  echo "Creating Firecracker VM init script..."

  # Create /sbin/ix-init - the Firecracker VM entry point
  # This script mounts filesystems, sets up networking, parses cmdline env vars, and starts ixd
  cp "${SCRIPT_DIR}/ix-init.sh" "${temp_dir}/sbin/ix-init"
INIT_SCRIPT_DONE=1

  chmod 755 "${temp_dir}/sbin/ix-init"
  echo "✓ ix-init script created"

  # Install the ix REPL script for stdin/stdout code execution
  sudo mkdir -p "${temp_dir}/usr/lib/ix"
  sudo cp "${SCRIPT_DIR}/../../daemon/crates/ix-code/src/ix_repl.py" "${temp_dir}/usr/lib/ix/repl.py"
  sudo chmod 644 "${temp_dir}/usr/lib/ix/repl.py"
  echo "✓ ix REPL script installed"
}

install_stage0() {
  local temp_dir="$1"

  if [[ ! -f "${SCRIPT_DIR}/ix-stage0.sh" ]]; then
    echo "Error: ix-stage0.sh not found at ${SCRIPT_DIR}/ix-stage0.sh" >&2
    return 1
  fi

  echo "Installing ix-stage0 (ro-rootfs overlay pre-init)..."
  sudo install -m 755 "${SCRIPT_DIR}/ix-stage0.sh" "${temp_dir}/sbin/ix-stage0"
  echo "✓ ix-stage0 installed"
}

install_browser_vm_init() {
  local temp_dir="$1"
  # Prefer the in-repo browser-vm-init over the (possibly stale) copy baked into
  # the Docker image, so init changes take effect on a rootfs rebuild without a
  # full image rebuild — mirrors how ix-init.sh is handled for the other tiers.
  local init_src="${SCRIPT_DIR}/../../daemon/cmd/browser-vm-init.sh"
  if [[ -f "$init_src" ]]; then
    sudo install -m 755 "$init_src" "${temp_dir}/usr/local/bin/browser-vm-init"
    echo "✓ browser-vm-init overlaid from repo (${init_src})"
  elif [[ ! -x "${temp_dir}/usr/local/bin/browser-vm-init" ]]; then
    echo "Error: browser-vm rootfs missing /usr/local/bin/browser-vm-init and no repo source at ${init_src}" >&2
    return 1
  fi
  # Symlink /sbin/ix-init -> browser-vm-init so a fixed kernel init= path works
  # for both tiers (kernel boot args use init=/sbin/ix-init). Use an ABSOLUTE
  # target: the Ubuntu base usr-merges /sbin -> /usr/sbin, so this link is
  # physically created in /usr/sbin/. A relative "../usr/local/bin/..." target
  # would resolve from /usr/sbin/ to the non-existent /usr/usr/local/bin/... and
  # dangle, making the kernel panic with "init failed (error -2)" at boot.
  sudo ln -sf /usr/local/bin/browser-vm-init "${temp_dir}/sbin/ix-init"
  echo "✓ browser-vm init linked at /sbin/ix-init"
}

create_directories() {
  local temp_dir="$1"

  echo "Creating required directories in rootfs..."
  sudo mkdir -p "${temp_dir}/run/ix"
  sudo mkdir -p "${temp_dir}/workspace"
  sudo mkdir -p "${temp_dir}/sbin"
  sudo mkdir -p "${temp_dir}/usr/local/bin"
  sudo mkdir -p "${temp_dir}/scratch"
  echo "✓ Directories created"
}

create_ext4_image() {
  local temp_dir="$1"
  local image_path="$2"
  local size_mb="$3"

  echo "Creating ext4 image (${size_mb} MB)..."

  # Create sparse image file
  dd if=/dev/zero of="$image_path" bs=1M count="$size_mb" 2>/dev/null || {
    echo "Error: Failed to create image file" >&2
    return 1
  }

  # Format as ext4
  mkfs.ext4 -F "$image_path" >/dev/null 2>&1 || {
    echo "Error: Failed to format image as ext4" >&2
    return 1
  }

  echo "✓ ext4 image created"

  # Mount and copy rootfs contents
  local mount_point
  mount_point="$(mktemp -d)"
  trap "sudo umount -l '$mount_point' 2>/dev/null || true; rm -rf '$mount_point'; cleanup" EXIT

  echo "Mounting image and copying rootfs contents..."
  sudo mount "$image_path" "$mount_point" || {
    echo "Error: Failed to mount image" >&2
    return 1
  }

  # Copy all rootfs contents to the mounted image
  sudo cp -a "${temp_dir}"/* "$mount_point/" || {
    echo "Error: Failed to copy rootfs contents" >&2
    return 1
  }

  echo "✓ Rootfs contents copied to ext4 image"

  # Unmount
  sudo umount -l "$mount_point" 2>/dev/null || true
  rm -rf "$mount_point"

  # Set proper permissions on image
  chmod 644 "$image_path"
  echo "✓ Image created: $image_path"
}

print_summary() {
  local image_path="$1"
  local tier="$2"
  local size_mb="$3"

  local actual_size
  actual_size=$(du -h "$image_path" | cut -f1)

  cat <<EOF

Rootfs image build complete!

Image:     $image_path
Tier:      $tier
Allocated: ${size_mb} MB
Actual:    $actual_size

Verify the image:

  file "$image_path"
  losetup -f
  sudo losetup /dev/loop0 "$image_path"
  sudo mount /dev/loop0 /mnt
  ls -la /mnt
  sudo umount /mnt
  sudo losetup -d /dev/loop0

Use with Firecracker:

  firecracker --config-file vm-config.json \\
    --root-drive /path/to/rootfs.ext4

EOF
}

main() {
  echo "Building rootfs ext4 image for tier: $TIER"
  echo "Output image: ${IX_ROOTFS_IMAGE}"
  echo "Image size: ${IX_ROOTFS_SIZE} MB"
  echo ""

  validate_tier "$TIER"
  ensure_output_dir "$IX_ROOTFS_IMAGE"

  # Create temporary rootfs extraction directory
  TEMP_ROOTFS="$(mktemp -d)" || {
    echo "Error: Failed to create temporary directory" >&2
    return 1
  }

  # Export Docker image to rootfs
  create_container_export "$IMAGE_TAG" "$TEMP_ROOTFS"

  # Populate rootfs
  create_directories "$TEMP_ROOTFS"
  install_stage0 "$TEMP_ROOTFS"
  if [[ "$TIER" == "browser-vm" ]]; then
    echo "browser-vm tier: skipping ixd + ix-init (init is browser-vm-init via ix-stage0)"
    install_browser_vm_init "$TEMP_ROOTFS"
  else
    copy_daemon_binary "$TEMP_ROOTFS"
    create_init_script "$TEMP_ROOTFS"
  fi

  # Create ext4 image
  create_ext4_image "$TEMP_ROOTFS" "$IX_ROOTFS_IMAGE" "$IX_ROOTFS_SIZE"

  echo ""
  print_summary "$IX_ROOTFS_IMAGE" "$TIER" "$IX_ROOTFS_SIZE"
}

main "$@"
