# ix Bundle Publishing — Design Spec

**Date:** 2026-06-15
**Status:** Approved (brainstorming) → ready for implementation plan

## Context

ix provides Firecracker microVM sandboxes. Today consumers **build the rootfs on
their deploy host**: pull the ix image, `docker export` it, run `mkfs.ext4`, copy
`ixd` in, and assemble `base.ext4`. That host-side build is the single largest
source of deploy failures.

This spec moves rootfs/bundle production into ix CI so consumers download a
prebuilt artifact instead of building one.

## Root cause: `latest` ships no `ixd`

`daemon/cmd/Dockerfile` **does** bake `ixd` — `COPY --from=builder /ixd
/usr/local/bin/ixd` in the `base` stage (currently ~line 49). But
`.github/workflows/build.yml` `build-push-action` sets **no `target:`**, so Docker
builds the **last** stage in the Dockerfile, `browser-vm` (stage 4), which is
standalone and deliberately omits `ixd`, Python, and Node. Therefore
`ghcr.io/nevindra/ix:latest` (= the default-branch build) contains no `ixd`.

Older published tags (`855856a`, `770ad8d`) predate the `browser-vm` stage, so
their last stage still carried `ixd` — which is why pinning those tags works
today as a stopgap.

## Goals

1. Publish ix Docker images **per stage** with explicit `target:` so each tier
   image contains exactly what it should (`base` carries `ixd`).
2. Publish a **rootfs bundle artifact** per release: a single archive with the
   firecracker binary, jailer, kernel, and the `base` rootfs `ext4` (with `ixd`
   baked) — everything a host needs under `/opt/ix`.
3. Own the full rootfs/init tooling in this repo so the artifact is reproducible
   from ix alone.

## Non-goals

- `browser-vm` bundle. Prepare the CI matrix but only the `base` bundle is
  required for now.
- Public artifacts. Releases stay private; consumers authenticate with a token.

## Design

### 1. Fix image publishing (`build.yml`)

Build images per stage with an explicit target, tagging each tier:

- `base`   → `ghcr.io/nevindra/ix:<tag>` and `:<tag>-base` (carries `ixd`)
- `browser`→ `:<tag>-browser`
- `browser-vm` → `:<tag>-browser-vm`

Use a build matrix over `{stage, suffix}`. The default/`latest` tag must point at
the `base` stage (the ixd-bearing image consumers expect), **not** `browser-vm`.

### 2. Bundle build + publish job

A CI job that runs on `v*` tags and produces
`ix-bundle-<ixver>-sdk<x.y>.tar.zst`:

```
build-bundle (needs: build-image):
  - download pinned firecracker + jailer  (ARG FC_VERSION)
  - download/build kernel vmlinux.bin     (ARG KERNEL_VERSION)
  - export the `base` image filesystem → mkfs.ext4 → assemble base.ext4
      (ixd already present in the image; add ix-init + ix-stage0)
  - tar (sparse-aware) + zstd → ix-bundle-<ixver>-sdk<x.y>.tar.zst
  - publish as a GitHub release asset on the v* tag
```

The bundle layout (extracts to `/opt/ix`):

```
/opt/ix/firecracker/firecracker
/opt/ix/firecracker/jailer
/opt/ix/firecracker/vmlinux.bin
/opt/ix/rootfs/base.ext4        # ixd baked, ix-init + ix-stage0 installed
```

### 3. Rootfs/init tooling in this repo

The rootfs assembly needs these scripts to live in ix and be driven by CI:

- `build-rootfs-ext4.sh`  → CI rootfs assembly step
- `install-firecracker.sh`→ CI firecracker/jailer fetch step
- `ix-init.sh`            → installed into the rootfs by CI
- `ix-stage0.sh`          → installed into the rootfs by CI
- `ix_repl.py`            → already sourced from `crates/ix-code/src/ix_repl.py`
  (Dockerfile line ~52); the rootfs build reuses that single source

Any consumer that currently vendors copies of these drops them once the bundle
ships.

## Version contract (critical)

`ixd` (in the rootfs) speaks a protocol consumed by `go-sdk`. They must not
drift:

- ix CI builds `ixd` and the rootfs **from the same commit**, so the binary and
  image cannot diverge.
- The bundle filename embeds both versions: `ix-bundle-<ixver>-sdk<x.y>.tar.zst`,
  where `<x.y>` is the `go-sdk` minor this `ixd` is compatible with.
- Consumers pin a bundle whose `sdk<x.y>` matches their `go-sdk` dependency.

## Risks & mitigations

1. **Bundle ↔ go-sdk mismatch (highest).** Mitigated by same-commit build +
   version-embedded filename + documented pairing.
2. **Bundle size.** zstd + sparse-aware tar; the rootfs ext4 is sparse.
3. **Release auth.** Private release; consumers use a token (same one used for
   GHCR).
4. **firecracker/kernel pinning.** Pinned via CI build ARGs (`FC_VERSION`,
   `KERNEL_VERSION`), single source of truth.

## Execution order

| # | Task | Model |
|---|------|-------|
| 1 | `build.yml`: per-stage targets + matrix; `latest` → `base` | `[sonnet-4.6]` |
| 2 | `build-bundle` CI job (fc + jailer + kernel + base.ext4 → tar.zst → release) | `[opus-4.7]` |
| 3 | Add rootfs/init scripts to this repo; wire into CI | `[sonnet-4.6]` |
| 4 | Cut a `v*` release → produce the first bundle artifact | `[sonnet-4.6]` |
