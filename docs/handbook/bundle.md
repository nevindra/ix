<!-- source-of-truth: .github/workflows/build.yml (build-bundle job), scripts/sandbox/{install-firecracker,build-rootfs-ext4,pack-bundle}.sh -->

# The ix Rootfs Bundle

The bundle is a single prebuilt artifact that gives a host everything it needs
to boot ix MicroVMs — **without building a rootfs locally**. Instead of running
Docker, exporting an image, and assembling an ext4 filesystem on every host, you
download one file, extract it to `/opt/ix`, and you are ready to run.

Each release of ix publishes one bundle as a GitHub release asset.

## What the bundle contains

The bundle is a `zstd`-compressed, sparse-aware tarball with two top-level
directories:

```
firecracker/
  firecracker      # the Firecracker VMM binary (pinned version)
  jailer           # the Firecracker jailer binary (pinned version)
  vmlinux.bin      # the pinned guest kernel
rootfs/
  base.ext4        # the base rootfs image, with ixd baked in
```

- `firecracker` / `jailer` — the pinned Firecracker VMM and its jailer, fetched
  from the upstream Firecracker release matching the bundle's `FC_VERSION`.
- `vmlinux.bin` — the pinned guest kernel the daemon boots VMs with.
- `base.ext4` — a sparse ext4 image of the base tier rootfs. It already contains
  the `ixd` daemon (`/usr/local/bin/ixd`) and the boot scripts
  (`/sbin/ix-init`, `/sbin/ix-stage0`), so no in-VM provisioning is needed.

Alongside each bundle, the release also carries a `.sha256` checksum file.

## Naming and the go-sdk compatibility contract

Bundles are named:

```
ix-bundle-<ixver>-sdk<x.y>.tar.zst
```

- `<ixver>` is the ix release tag the bundle was cut from (e.g. `v0.2.1`). This
  identifies the exact `ixd` build baked into `base.ext4`.
- `<x.y>` is the **go-sdk compatibility minor** — the `go-sdk` minor version this
  `ixd` speaks the wire protocol with.

**The contract:** a bundle named `ix-bundle-<ixver>-sdk<x.y>` is guaranteed to
interoperate with any `go-sdk` whose version is `x.y.*`. Match the `sdk<x.y>`
segment of the bundle to the `go-sdk` minor your application pins. If they
diverge across a minor boundary, the daemon and SDK may disagree on the wire
protocol — pick a bundle whose `sdk<x.y>` equals your SDK's minor.

The `<ixver>` and `sdk<x.y>` versions move independently: a patch bump to `ixd`
produces a new `<ixver>` while `sdk<x.y>` stays the same, so consumers on a given
SDK minor can take daemon patches without changing their SDK pin.

## How to download

Bundles are GitHub release assets. If the repository (and therefore its
releases) is private, you need a token with `read` access to repository contents.

With the `gh` CLI (it reads your token from `gh auth`):

```bash
gh release download <ixver> \
  -p 'ix-bundle-*.tar.zst' \
  -p '*.sha256'
```

For a private release without `gh`, use a personal access token directly. Look
up the asset's API URL, then fetch it with the Octet-stream `Accept` header so
the API streams the binary rather than JSON metadata:

```bash
TOKEN=<your-token>
# Find the asset download URL from the release API, then:
curl -fsSL \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Accept: application/octet-stream" \
  -o ix-bundle-<ixver>-sdk<x.y>.tar.zst \
  "<asset-api-url>"
```

Always download the matching `.sha256` and verify it before extracting (see
below).

## Expected `/opt/ix` layout after extraction

Extract the bundle into `/opt/ix`:

```bash
sudo mkdir -p /opt/ix
zstd -dc ix-bundle-<ixver>-sdk<x.y>.tar.zst | sudo tar -C /opt/ix -xf -
```

After extraction, `/opt/ix` looks like:

```
/opt/ix/
  firecracker/
    firecracker
    jailer
    vmlinux.bin
  rootfs/
    base.ext4
```

This is the layout the daemon expects by default. The tarball is unpacked
sparse-aware, so `base.ext4` keeps its on-disk sparseness.

## Verifying the bundle

Run these checks after downloading and extracting. They confirm the checksum,
the on-disk layout, and that `ixd` is present inside the rootfs.

Checksum and layout:

```bash
cd /tmp && rm -rf opt-ix && mkdir opt-ix
sha256sum -c ix-bundle-<ixver>-sdk<x.y>.tar.zst.sha256
zstd -dc ix-bundle-<ixver>-sdk<x.y>.tar.zst | tar -C opt-ix -xf -
test -x opt-ix/firecracker/firecracker
test -f opt-ix/firecracker/vmlinux.bin
test -f opt-ix/rootfs/base.ext4
echo "bundle layout OK"
```

Expected: `bundle layout OK`.

`ixd` inside the rootfs:

```bash
sudo mkdir -p /mnt/ck && sudo mount /tmp/opt-ix/rootfs/base.ext4 /mnt/ck
test -x /mnt/ck/usr/local/bin/ixd && echo "ixd present"
sudo umount /mnt/ck
```

Expected: `ixd present`.
