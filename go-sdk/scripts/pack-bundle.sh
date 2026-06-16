#!/usr/bin/env bash
set -euo pipefail

IX_VERSION="${1:?ix version, e.g. v0.2.1}"
SDK_COMPAT="${2:?go-sdk compat minor, e.g. 0.2}"
SRC="${3:-/opt/ix}"                       # tree with firecracker/ and rootfs/
OUT_DIR="${4:-dist}"

mkdir -p "$OUT_DIR"
name="ix-bundle-${IX_VERSION}-sdk${SDK_COMPAT}.tar.zst"

# sparse-aware tar; zstd -19 for size. base.ext4 is sparse.
tar --sparse -C "$SRC" -cf - firecracker rootfs \
  | zstd -19 -T0 -o "$OUT_DIR/$name"

echo "==> $OUT_DIR/$name"
ls -la "$OUT_DIR/$name"
sha256sum "$OUT_DIR/$name" | tee "$OUT_DIR/$name.sha256"
