#!/bin/bash
set -euo pipefail

TIER="${1:-base}"
OUT="${IX_ROOTFS_OUT:-/opt/ix/rootfs/${TIER}}"

readonly VALID_TIERS=("base" "browser")
readonly IMAGE_TAG="ix:${TIER}"
readonly DAEMON_BIN="target/release/ix"

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

create_output_dir() {
  local dir="$1"
  if [[ ! -d "$dir" ]]; then
    mkdir -p "$dir" || sudo mkdir -p "$dir"
    if [[ ! -w "$dir" ]]; then
      sudo chown "$(id -u):$(id -g)" "$dir"
    fi
  fi
}

main() {
  validate_tier "$TIER"

  echo "Building rootfs for tier: $TIER"
  echo "Output directory: $OUT"

  create_output_dir "$OUT"

  local container_id
  echo "Creating temporary container from $IMAGE_TAG..."
  container_id=$(docker create "$IMAGE_TAG")
  trap "docker rm -f '$container_id' >/dev/null 2>&1 || true" EXIT

  echo "Exporting container filesystem..."
  docker export "$container_id" | tar -xf - -C "$OUT"

  if [[ -f "$DAEMON_BIN" ]]; then
    echo "Copying ix daemon binary..."
    sudo install -m 755 "$DAEMON_BIN" "$OUT/usr/bin/ix"
  else
    echo "Warning: $DAEMON_BIN not found, skipping daemon copy"
  fi

  echo "Ensuring /run/ix directory exists in rootfs..."
  sudo mkdir -p "$OUT/run/ix"

  echo "Rootfs build complete: $OUT"
}

main "$@"
