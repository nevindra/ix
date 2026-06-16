# ix Bundle Publishing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make ix CI publish ixd-bearing per-stage images and a single prebuilt rootfs bundle artifact, so consumers download `/opt/ix` instead of building a rootfs on the host.

**Architecture:** Fix `build.yml` to build each Dockerfile stage with an explicit `target:` (so `latest`/`base` carries `ixd`). Add the rootfs/init tooling to this repo. Add a `build-bundle` CI job that, on `v*` tags, assembles `firecracker + jailer + vmlinux.bin + base.ext4` (ixd baked) into `ix-bundle-<ixver>-sdk<x.y>.tar.zst` and publishes it as a GitHub release asset.

**Tech Stack:** GitHub Actions, Docker Buildx, bash, `mkfs.ext4`/`e2fsprogs`, zstd, Firecracker.

**Spec:** `docs/superpowers/specs/2026-06-15-ix-bundle-publishing-design.md`

---

## File Structure

- `.github/workflows/build.yml` — MODIFY: per-stage image matrix; ADD `build-bundle` job.
- `scripts/sandbox/install-firecracker.sh` — CREATE: fetch pinned firecracker + jailer + kernel.
- `scripts/sandbox/build-rootfs-ext4.sh` — CREATE: export `base` image → assemble `base.ext4`.
- `scripts/sandbox/ix-init.sh` — CREATE: rootfs PID 1 init (per-tier).
- `scripts/sandbox/ix-stage0.sh` — CREATE: ro-rootfs overlay pre-init.
- `scripts/sandbox/pack-bundle.sh` — CREATE: assemble `/opt/ix` tree → `tar.zst`.
- `docs/handbook/bundle.md` — CREATE: consumer-facing "how to use the bundle" doc.

> Source scripts to port already exist in a downstream consumer's
> `scripts/sandbox/` tree. Copy them verbatim into this repo, then adapt paths
> as the steps specify. They are reproduced inline below so this plan stands
> alone.

---

## Task 1: Fix image publishing — per-stage targets

**Files:**
- Modify: `.github/workflows/build.yml` (the `build-image` job)

- [ ] **Step 1: Inspect current behaviour**

Run: `grep -n "target\|tags:\|build-push" .github/workflows/build.yml`
Expected: no `target:` key under `build-push-action` — confirms the bug (last
stage `browser-vm` gets published, which has no `ixd`).

- [ ] **Step 2: Replace `build-image` job with a per-stage matrix**

Replace the entire `build-image` job in `.github/workflows/build.yml` with:

```yaml
  build-image:
    name: Docker Image (${{ matrix.stage }})
    needs: [test-daemon]
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include:
          - stage: base
            suffix: ""          # base is the default image (and :latest)
          - stage: browser
            suffix: "-browser"
          - stage: browser-vm
            suffix: "-browser-vm"
    steps:
      - uses: actions/checkout@v5

      - name: Download daemon binary
        uses: actions/download-artifact@v4
        with:
          name: ixd-linux-amd64
          path: daemon/target/x86_64-unknown-linux-musl/release/

      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@v3

      - name: Log in to GHCR
        if: github.event_name != 'pull_request'
        uses: docker/login-action@v3
        with:
          registry: ${{ env.REGISTRY }}
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Extract metadata
        id: meta
        uses: docker/metadata-action@v5
        with:
          images: ${{ env.REGISTRY }}/${{ env.IMAGE_NAME }}
          flavor: |
            latest=false
            suffix=${{ matrix.suffix }},onlatest=true
          tags: |
            type=semver,pattern={{version}}
            type=semver,pattern={{major}}.{{minor}}
            type=sha,prefix=
            type=raw,value=latest,enable=${{ matrix.stage == 'base' && github.ref == format('refs/heads/{0}', github.event.repository.default_branch) }}

      - name: Build and push
        uses: docker/build-push-action@v6
        with:
          context: daemon
          file: daemon/cmd/Dockerfile
          target: ${{ matrix.stage }}
          push: ${{ github.event_name != 'pull_request' }}
          tags: ${{ steps.meta.outputs.tags }}
          labels: ${{ steps.meta.outputs.labels }}
          cache-from: type=gha,scope=ix-${{ matrix.stage }}
          cache-to: type=gha,mode=max,scope=ix-${{ matrix.stage }}
```

Key changes: `target: ${{ matrix.stage }}`; `:latest` enabled **only** for the
`base` stage on the default branch; per-stage cache scopes; tier suffix on
non-base images.

- [ ] **Step 3: Validate the workflow file parses**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/build.yml')); print('YAML OK')"`
Expected: `YAML OK`

- [ ] **Step 4: Open a PR and verify the matrix builds**

Push the branch, open a PR. On the PR run, confirm three `build-image` matrix
jobs run (`base`, `browser`, `browser-vm`) and the `base` one builds the stage
containing `ixd`.

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/build.yml
git commit -m "ci: build ix images per stage; latest tracks ixd-bearing base"
```

---

## Task 2: Add firecracker/kernel fetch script

**Files:**
- Create: `scripts/sandbox/install-firecracker.sh`

- [ ] **Step 1: Create the script**

Create `scripts/sandbox/install-firecracker.sh`. It downloads pinned
firecracker + jailer and a pinned guest kernel into `$IX_FC_DIR` (default
`/opt/ix/firecracker`). Pin versions via args so CI is the single source of
truth.

```bash
#!/usr/bin/env bash
set -euo pipefail

FC_VERSION="${1:-1.15.1}"
KERNEL_URL="${2:-}"                       # required: pinned vmlinux.bin URL
ARCH="$(uname -m)"
IX_FC_DIR="${IX_FC_DIR:-/opt/ix/firecracker}"
TEMP_DIR=""

[[ -n "$KERNEL_URL" ]] || { echo "Error: kernel URL (arg 2) required" >&2; exit 1; }

cleanup() { [[ -n "$TEMP_DIR" && -d "$TEMP_DIR" ]] && rm -rf "$TEMP_DIR"; }
trap cleanup EXIT

case "$ARCH" in x86_64|aarch64) ;; *) echo "Unsupported arch: $ARCH" >&2; exit 1;; esac

mkdir -p "$IX_FC_DIR"
TEMP_DIR="$(mktemp -d)"

echo "==> firecracker v${FC_VERSION} (${ARCH})"
curl -fsSL -o "$TEMP_DIR/fc.tgz" \
  "https://github.com/firecracker-microvm/firecracker/releases/download/v${FC_VERSION}/firecracker-v${FC_VERSION}-${ARCH}.tgz"
tar -xzf "$TEMP_DIR/fc.tgz" -C "$TEMP_DIR"
install -m755 "$TEMP_DIR/release-v${FC_VERSION}-${ARCH}/firecracker-v${FC_VERSION}-${ARCH}" "$IX_FC_DIR/firecracker"
install -m755 "$TEMP_DIR/release-v${FC_VERSION}-${ARCH}/jailer-v${FC_VERSION}-${ARCH}"     "$IX_FC_DIR/jailer"

echo "==> guest kernel"
curl -fsSL -o "$IX_FC_DIR/vmlinux.bin" "$KERNEL_URL"

echo "==> installed:"
ls -la "$IX_FC_DIR"
```

- [ ] **Step 2: Lint**

Run: `bash -n scripts/sandbox/install-firecracker.sh && echo "syntax OK"`
Expected: `syntax OK`

- [ ] **Step 3: Smoke test locally (x86_64 host with internet)**

Run: `IX_FC_DIR=/tmp/ixfc bash scripts/sandbox/install-firecracker.sh 1.15.1 "<pinned-vmlinux-url>"`
Expected: `/tmp/ixfc/firecracker`, `/tmp/ixfc/jailer`, `/tmp/ixfc/vmlinux.bin` all present; `/tmp/ixfc/firecracker --version` prints `v1.15.1`.

- [ ] **Step 4: Commit**

```bash
git add scripts/sandbox/install-firecracker.sh
git commit -m "feat(sandbox): firecracker + jailer + kernel fetch script"
```

---

## Task 3: Add rootfs init scripts (ix-init, ix-stage0)

**Files:**
- Create: `scripts/sandbox/ix-init.sh`
- Create: `scripts/sandbox/ix-stage0.sh`

- [ ] **Step 1: Add `ix-stage0.sh`**

Create `scripts/sandbox/ix-stage0.sh` — pre-init that overlays the read-only
shared rootfs (`/dev/vda`) with the per-VM scratch disk (`/dev/vdb`), then execs
the tier init. Copy the downstream version verbatim. Header for reference:

```sh
#!/bin/sh
# ix-stage0 — pre-init for ALL ix VM tiers (base, browser, browser-vm).
# Rootfs (/dev/vda) is attached READ-ONLY; all writes go to the per-VM scratch
# disk (/dev/vdb) via a whole-root overlayfs. Kernel args:
#   root=/dev/vda ro init=/sbin/ix-stage0
set -e
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true
# ... (full overlay setup + exec of /sbin/ix-init) ...
```

> Reproduce the complete script from the source repo. Do not summarise — the
> overlay mount sequence and final `exec` are load-bearing.

- [ ] **Step 2: Add `ix-init.sh`**

Create `scripts/sandbox/ix-init.sh` — PID 1 init inside the VM: mounts
`/proc`/`/sys`/`/dev`, sets `PATH`, brings up networking, parses kernel cmdline,
and starts `ixd`. Copy verbatim. Header for reference:

```bash
#!/bin/bash
# ix-init: PID 1 init script for Firecracker MicroVM
echo "ix-init: Starting..."
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
mountpoint -q /proc || mount -t proc proc /proc
# ... (full mounts + net + cmdline parse + ixd start) ...
```

- [ ] **Step 3: Lint both**

Run: `bash -n scripts/sandbox/ix-init.sh && sh -n scripts/sandbox/ix-stage0.sh && echo "syntax OK"`
Expected: `syntax OK`

- [ ] **Step 4: Commit**

```bash
git add scripts/sandbox/ix-init.sh scripts/sandbox/ix-stage0.sh
git commit -m "feat(sandbox): rootfs ix-init + ix-stage0 boot scripts"
```

---

## Task 4: Add rootfs assembly script

**Files:**
- Create: `scripts/sandbox/build-rootfs-ext4.sh`

- [ ] **Step 1: Create the assembly script**

Create `scripts/sandbox/build-rootfs-ext4.sh`. It exports the `base` image
filesystem, installs `ix-init` + `ix-stage0` (and reuses the `ixd` already baked
in the image), and writes a sparse `base.ext4`. The `ixd` source is the image —
there is **no** `IX_REPO`/cargo path in CI (the image already contains it).

```bash
#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TIER="${1:-base}"
IMAGE_TAG="${IMAGE_TAG:-ix:base}"                     # the ixd-bearing image
OUT="${IX_ROOTFS_IMAGE:-/opt/ix/rootfs/${TIER}.ext4}"
SIZE_MB="${IX_ROOTFS_SIZE:-4096}"
TMP=""; MNT=""

cleanup() {
  [[ -n "$MNT" ]] && mountpoint -q "$MNT" && sudo umount -l "$MNT" || true
  [[ -n "$MNT" ]] && rm -rf "$MNT"
  [[ -n "$TMP" ]] && sudo rm -rf "$TMP"
}
trap cleanup EXIT

mkdir -p "$(dirname "$OUT")"
TMP="$(mktemp -d)"

echo "==> export $IMAGE_TAG"
cid="$(docker create "$IMAGE_TAG")"
docker export "$cid" | sudo tar -xf - -C "$TMP"
docker rm -f "$cid" >/dev/null

echo "==> verify ixd present in image"
[[ -x "$TMP/usr/local/bin/ixd" ]] || { echo "ERROR: no ixd in $IMAGE_TAG" >&2; exit 1; }

echo "==> install boot scripts"
sudo install -m755 "$SCRIPT_DIR/ix-stage0.sh" "$TMP/sbin/ix-stage0"
sudo install -m755 "$SCRIPT_DIR/ix-init.sh"   "$TMP/sbin/ix-init"
sudo mkdir -p "$TMP/run/ix" "$TMP/workspace" "$TMP/scratch"

echo "==> build ext4 ($SIZE_MB MB)"
dd if=/dev/zero of="$OUT" bs=1M count="$SIZE_MB" 2>/dev/null
mkfs.ext4 -F "$OUT" >/dev/null 2>&1
MNT="$(mktemp -d)"
sudo mount "$OUT" "$MNT"
sudo cp -a "$TMP"/* "$MNT/"
sudo umount -l "$MNT"
chmod 644 "$OUT"
echo "==> done: $OUT"
```

- [ ] **Step 2: Lint**

Run: `bash -n scripts/sandbox/build-rootfs-ext4.sh && echo "syntax OK"`
Expected: `syntax OK`

- [ ] **Step 3: Local end-to-end test (x86_64 host with docker + sudo)**

```bash
docker build -f daemon/cmd/Dockerfile --target base -t ix:base daemon
IX_ROOTFS_IMAGE=/tmp/base.ext4 bash scripts/sandbox/build-rootfs-ext4.sh base
```
Expected: `/tmp/base.ext4` created. Verify `ixd` landed:
```bash
sudo mkdir -p /mnt/ck && sudo mount /tmp/base.ext4 /mnt/ck && ls -l /mnt/ck/usr/local/bin/ixd /mnt/ck/sbin/ix-init /mnt/ck/sbin/ix-stage0 && sudo umount /mnt/ck
```
Expected: all three files listed.

- [ ] **Step 4: Commit**

```bash
git add scripts/sandbox/build-rootfs-ext4.sh
git commit -m "feat(sandbox): assemble base.ext4 from the ixd-bearing image"
```

---

## Task 5: Bundle packing script

**Files:**
- Create: `scripts/sandbox/pack-bundle.sh`

- [ ] **Step 1: Create the packer**

Create `scripts/sandbox/pack-bundle.sh`. It takes a populated `/opt/ix`-style
tree and produces `ix-bundle-<ixver>-sdk<x.y>.tar.zst`. Versions are passed in
so CI controls naming.

```bash
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
```

- [ ] **Step 2: Lint**

Run: `bash -n scripts/sandbox/pack-bundle.sh && echo "syntax OK"`
Expected: `syntax OK`

- [ ] **Step 3: Local test**

```bash
mkdir -p /tmp/ix/firecracker /tmp/ix/rootfs
cp /tmp/ixfc/* /tmp/ix/firecracker/ 2>/dev/null || true
cp /tmp/base.ext4 /tmp/ix/rootfs/
bash scripts/sandbox/pack-bundle.sh v0.0.0 0.2 /tmp/ix /tmp/dist
```
Expected: `/tmp/dist/ix-bundle-v0.0.0-sdk0.2.tar.zst` + `.sha256` exist. Round-trip:
```bash
mkdir -p /tmp/unp && zstd -dc /tmp/dist/ix-bundle-v0.0.0-sdk0.2.tar.zst | tar -C /tmp/unp -xf - && find /tmp/unp -maxdepth 2
```
Expected: `firecracker/{firecracker,jailer,vmlinux.bin}` and `rootfs/base.ext4`.

- [ ] **Step 4: Commit**

```bash
git add scripts/sandbox/pack-bundle.sh
git commit -m "feat(sandbox): pack /opt/ix tree into versioned tar.zst bundle"
```

---

## Task 6: `build-bundle` CI job

**Files:**
- Modify: `.github/workflows/build.yml` (add job + version ARGs)

- [ ] **Step 1: Add pinned versions to workflow env**

In `.github/workflows/build.yml`, extend the top-level `env:` block:

```yaml
env:
  REGISTRY: ghcr.io
  IMAGE_NAME: ${{ github.repository }}
  FC_VERSION: "1.15.1"
  KERNEL_URL: "https://<pinned-host>/vmlinux-6.1.bin"   # single source of truth
  SDK_COMPAT: "0.2"                                       # go-sdk minor ixd targets
```

- [ ] **Step 2: Add the `build-bundle` job**

Append to the `jobs:` map in `.github/workflows/build.yml`:

```yaml
  build-bundle:
    name: Rootfs bundle
    needs: [build-image]
    if: startsWith(github.ref, 'refs/tags/v')
    runs-on: ubuntu-latest
    permissions:
      contents: write          # create release + upload asset
      packages: read
    steps:
      - uses: actions/checkout@v5

      - name: Tools (e2fsprogs, zstd)
        run: sudo apt-get update && sudo apt-get install -y e2fsprogs zstd

      - name: Log in to GHCR
        uses: docker/login-action@v3
        with:
          registry: ${{ env.REGISTRY }}
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Pull base image (this tag)
        run: |
          docker pull "${REGISTRY}/${IMAGE_NAME}:${GITHUB_REF_NAME}"
          docker tag  "${REGISTRY}/${IMAGE_NAME}:${GITHUB_REF_NAME}" ix:base

      - name: Firecracker + kernel
        run: |
          sudo IX_FC_DIR=/opt/ix/firecracker \
            bash scripts/sandbox/install-firecracker.sh "${FC_VERSION}" "${KERNEL_URL}"

      - name: Assemble rootfs
        run: |
          sudo IMAGE_TAG=ix:base IX_ROOTFS_IMAGE=/opt/ix/rootfs/base.ext4 \
            bash scripts/sandbox/build-rootfs-ext4.sh base

      - name: Pack bundle
        run: |
          sudo bash scripts/sandbox/pack-bundle.sh \
            "${GITHUB_REF_NAME}" "${SDK_COMPAT}" /opt/ix dist
          ls -la dist

      - name: Publish release asset
        uses: softprops/action-gh-release@v2
        with:
          files: |
            dist/*.tar.zst
            dist/*.sha256
```

- [ ] **Step 3: Validate YAML**

Run: `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/build.yml')); print('YAML OK')"`
Expected: `YAML OK`

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/build.yml
git commit -m "ci: build + publish rootfs bundle artifact on v* tags"
```

---

## Task 7: Cut a release and verify the bundle

**Files:** none (release operation)

- [ ] **Step 1: Tag a release**

```bash
git tag v0.2.1
git push origin v0.2.1
```

- [ ] **Step 2: Watch the run**

In Actions, confirm `build-image` (base/browser/browser-vm) then `build-bundle`
succeed, and a release `v0.2.1` is created with
`ix-bundle-v0.2.1-sdk0.2.tar.zst` + `.sha256` attached.

- [ ] **Step 3: Verify the published bundle on a clean host**

```bash
cd /tmp && rm -rf opt-ix && mkdir opt-ix
gh release download v0.2.1 -R nevindra/ix -p 'ix-bundle-*.tar.zst' -p '*.sha256'
sha256sum -c ix-bundle-v0.2.1-sdk0.2.tar.zst.sha256
zstd -dc ix-bundle-v0.2.1-sdk0.2.tar.zst | tar -C opt-ix -xf -
test -x opt-ix/firecracker/firecracker
test -f opt-ix/firecracker/vmlinux.bin
test -f opt-ix/rootfs/base.ext4
echo "bundle layout OK"
```
Expected: `bundle layout OK`.

- [ ] **Step 4: Verify ixd inside the rootfs**

```bash
sudo mkdir -p /mnt/ck && sudo mount /tmp/opt-ix/rootfs/base.ext4 /mnt/ck
test -x /mnt/ck/usr/local/bin/ixd && echo "ixd present"
sudo umount /mnt/ck
```
Expected: `ixd present`.

---

## Task 8: Consumer doc

**Files:**
- Create: `docs/handbook/bundle.md`

- [ ] **Step 1: Write the doc**

Create `docs/handbook/bundle.md` covering: what the bundle contains, the
`ix-bundle-<ixver>-sdk<x.y>` naming + the go-sdk compat contract, how to
download (with a token for the private release), and the expected `/opt/ix`
layout after extraction. Include the verification commands from Task 7.

- [ ] **Step 2: Commit**

```bash
git add docs/handbook/bundle.md
git commit -m "docs: how to consume the ix rootfs bundle"
```

---

## Self-Review Notes

- **Spec coverage:** Goal 1 (per-stage images) → Task 1. Goal 2 (bundle artifact)
  → Tasks 2–7. Goal 3 (own rootfs tooling) → Tasks 2–5. Version contract →
  Tasks 5/6 (`SDK_COMPAT`, embedded filename) + Task 8 doc.
- **Pins:** `FC_VERSION`, `KERNEL_URL`, `SDK_COMPAT` live only in workflow `env:`.
- **Open item to confirm before Task 6:** the pinned `KERNEL_URL` host. Use the
  same kernel the current deploy uses; record it in the workflow `env:`.
