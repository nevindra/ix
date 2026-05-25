#!/bin/bash
set -euo pipefail

VERSION="${1:-1.15.1}"
ARCH="$(uname -m)"
IX_FC_DIR="${IX_FC_DIR:-/opt/ix/firecracker}"
TEMP_DIR=""

readonly GITHUB_REPO="firecracker-microvm/firecracker"
readonly FIRECRACKER_BIN_NAME="firecracker"
readonly JAILER_BIN_NAME="jailer"

cleanup() {
  if [[ -n "$TEMP_DIR" && -d "$TEMP_DIR" ]]; then
    rm -rf "$TEMP_DIR"
  fi
}

trap cleanup EXIT

validate_arch() {
  case "$ARCH" in
    x86_64)
      return 0
      ;;
    aarch64)
      return 0
      ;;
    *)
      echo "Error: Unsupported architecture: $ARCH" >&2
      return 1
      ;;
  esac
}

ensure_dir() {
  local dir="$1"
  if [[ ! -d "$dir" ]]; then
    mkdir -p "$dir" || sudo mkdir -p "$dir"
  fi
  if [[ ! -w "$dir" ]]; then
    sudo chown "$(id -u):$(id -g)" "$dir" || {
      echo "Error: Cannot write to $dir" >&2
      return 1
    }
  fi
}

download_firecracker() {
  local version="$1"
  local arch="$2"
  local temp_dir="$3"

  echo "Downloading Firecracker v${version} for ${arch}..."

  # Map uname arch to Firecracker release arch
  local fc_arch="$arch"
  if [[ "$arch" == "x86_64" ]]; then
    fc_arch="x86_64"
  elif [[ "$arch" == "aarch64" ]]; then
    fc_arch="aarch64"
  fi

  local url="https://github.com/${GITHUB_REPO}/releases/download/v${version}/firecracker-v${version}-${fc_arch}.tgz"

  if ! curl -fL "$url" -o "${temp_dir}/firecracker.tgz"; then
    echo "Error: Failed to download Firecracker binary from $url" >&2
    return 1
  fi

  tar -tzf "${temp_dir}/firecracker.tgz" > /dev/null || {
    echo "Error: Downloaded file is not a valid tar.gz" >&2
    return 1
  }

  tar -xzf "${temp_dir}/firecracker.tgz" -C "$temp_dir" || {
    echo "Error: Failed to extract Firecracker archive" >&2
    return 1
  }

  echo "✓ Firecracker binary downloaded and extracted"
}

download_kernel() {
  local version="$1"
  local arch="$2"
  local temp_dir="$3"

  echo "Downloading kernel from Firecracker CI S3 bucket..."

  # Firecracker no longer ships kernels in GitHub releases.
  # Pre-built kernels are in their CI S3 bucket.
  # Extract major.minor from version (e.g., 1.15.1 -> v1.15)
  local fc_minor="v$(echo "$version" | cut -d. -f1-2)"
  local s3_prefix="firecracker-ci/${fc_minor}/${arch}"
  local s3_bucket="https://s3.amazonaws.com/spec.ccfc.min"

  # Use list-type=2 API and match only direct vmlinux entries (not debug/)
  local kernel_key
  kernel_key=$(curl -sL "${s3_bucket}/?prefix=${s3_prefix}/vmlinux-&list-type=2" \
    | grep -oP "(?<=<Key>)(${s3_prefix}/vmlinux-[0-9]+\.[0-9]+\.[0-9]{1,3})(?=</Key>)" \
    | sort -V | tail -1)

  if [[ -z "$kernel_key" ]]; then
    echo "Warning: No kernel found in S3 for ${s3_prefix}. You may need to build one." >&2
    return 0
  fi

  local kernel_name
  kernel_name=$(basename "$kernel_key")
  local url="${s3_bucket}/${kernel_key}"

  echo "  Found: ${kernel_name}"
  if ! curl -fL "$url" -o "${temp_dir}/${kernel_name}"; then
    echo "Warning: Kernel download failed from $url" >&2
    return 0
  fi

  echo "✓ Kernel downloaded: ${kernel_name}"
}

install_binaries() {
  local temp_dir="$1"
  local install_dir="$2"

  echo "Installing Firecracker binaries to ${install_dir}..."

  ensure_dir "$install_dir"

  # Archive structure: release-v{VERSION}-{ARCH}/firecracker-v{VERSION}-{ARCH}
  local release_dir="${temp_dir}/release-v${VERSION}-${ARCH}"
  local firecracker_src=""
  local jailer_src=""

  if [[ -f "${release_dir}/firecracker-v${VERSION}-${ARCH}" ]]; then
    firecracker_src="${release_dir}/firecracker-v${VERSION}-${ARCH}"
  elif [[ -f "${temp_dir}/firecracker-v${VERSION}-${ARCH}" ]]; then
    firecracker_src="${temp_dir}/firecracker-v${VERSION}-${ARCH}"
  else
    echo "Error: Firecracker binary not found in archive. Contents:" >&2
    find "$temp_dir" -type f -name 'firecracker*' 2>/dev/null >&2
    return 1
  fi

  if [[ -f "${release_dir}/jailer-v${VERSION}-${ARCH}" ]]; then
    jailer_src="${release_dir}/jailer-v${VERSION}-${ARCH}"
  elif [[ -f "${temp_dir}/jailer-v${VERSION}-${ARCH}" ]]; then
    jailer_src="${temp_dir}/jailer-v${VERSION}-${ARCH}"
  else
    echo "Warning: Jailer binary not found in archive" >&2
  fi

  # Copy and rename binaries
  if [[ -f "$firecracker_src" ]]; then
    cp "$firecracker_src" "${install_dir}/${FIRECRACKER_BIN_NAME}"
    chmod 755 "${install_dir}/${FIRECRACKER_BIN_NAME}"
    echo "  ✓ Installed ${FIRECRACKER_BIN_NAME}"
  fi

  if [[ -f "$jailer_src" ]]; then
    cp "$jailer_src" "${install_dir}/${JAILER_BIN_NAME}"
    chmod 755 "${install_dir}/${JAILER_BIN_NAME}"
    echo "  ✓ Installed ${JAILER_BIN_NAME}"
  fi

  # Install kernel if downloaded — rename to vmlinux.bin for consistency
  local kernel_installed=false
  for kernel_file in "${temp_dir}"/vmlinux*; do
    if [[ -f "$kernel_file" ]]; then
      cp "$kernel_file" "${install_dir}/vmlinux.bin"
      chmod 644 "${install_dir}/vmlinux.bin"
      echo "  ✓ Installed vmlinux.bin (from $(basename "$kernel_file"))"
      kernel_installed=true
      break
    fi
  done
  if [[ "$kernel_installed" != "true" ]]; then
    echo "  ⚠ No kernel installed — set IX_KERNEL_PATH manually"
  fi
}

print_env_suggestions() {
  local install_dir="$1"

  cat <<EOF

Installation complete!

Add these environment variables to your shell profile:

  export IX_FC_BINARY="${install_dir}/${FIRECRACKER_BIN_NAME}"
  export IX_KERNEL_PATH="${install_dir}/vmlinux.bin"

Verify:

  ${install_dir}/${FIRECRACKER_BIN_NAME} --version
  ls -lh ${install_dir}/

EOF
}

main() {
  echo "Installing Firecracker v${VERSION} (${ARCH})"
  echo "Installation directory: ${IX_FC_DIR}"
  echo ""

  validate_arch || return 1

  TEMP_DIR="$(mktemp -d)" || {
    echo "Error: Failed to create temporary directory" >&2
    return 1
  }

  download_firecracker "$VERSION" "$ARCH" "$TEMP_DIR" || return 1
  download_kernel "$VERSION" "$ARCH" "$TEMP_DIR" || return 1
  install_binaries "$TEMP_DIR" "$IX_FC_DIR" || return 1

  echo ""
  print_env_suggestions "$IX_FC_DIR"
}

main "$@"
