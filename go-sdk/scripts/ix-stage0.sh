#!/bin/sh
# ix-stage0 — pre-init for ALL ix VM tiers (base, browser, browser-vm).
#
# The rootfs (/dev/vda) is attached READ-ONLY at the Firecracker level: it is
# one shared image for every VM on the host. All writes go to the per-VM
# scratch disk (/dev/vdb) through a whole-root overlayfs, so the per-tier init
# scripts (ix-init / browser-vm-init) need no changes — their writes land in
# the overlay upper layer transparently.
#
# Kernel boot args: root=/dev/vda ro init=/sbin/ix-stage0
#
# Any failure here exits PID 1 → kernel panic (panic=1) → Firecracker exits →
# the host surfaces a boot/health timeout. Debug with IX_VM_CONSOLE=1.

set -e

# The docker-exported rootfs has no static device nodes; /dev/vdb only
# appears once devtmpfs is mounted. `|| true` tolerates kernels that
# auto-mount devtmpfs (CONFIG_DEVTMPFS_MOUNT).
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true

# Per-VM writable scratch disk. The /scratch mountpoint is baked into the
# image by build-rootfs-ext4.sh. noatime: every read would otherwise write
# an atime update to the scratch on the agent's file-op hot path.
mount -o noatime /dev/vdb /scratch

mkdir -p /scratch/upper /scratch/work /scratch/newroot /scratch/workspace
mount -t overlay overlay \
  -o lowerdir=/,upperdir=/scratch/upper,workdir=/scratch/work \
  /scratch/newroot

# /workspace hot path: bind the scratch's own directory OVER the overlay's
# /workspace so agent file ops write raw ext4 — no overlayfs lookup/copy-up
# machinery in the write path. Everything else (pip installs, /etc writes)
# still goes through the whole-root overlay. mkdir writes the overlay upper,
# guaranteeing the mountpoint exists regardless of the base image.
mkdir -p /scratch/newroot/workspace
mount --bind /scratch/workspace /scratch/newroot/workspace

# Pivot into the overlay. put_old must exist inside the new root; mkdir here
# writes to the upper layer (the overlay root is writable).
cd /scratch/newroot
mkdir -p old_root
pivot_root . old_root

# Re-anchor root in the overlay FIRST: pivot_root does not retarget this
# shell's root on the guest kernel (observed on 6.1.155: resolving /old_root
# afterwards fails with ENOENT against the stale ro root), so the old-root
# detach must happen INSIDE the chroot, where / is the overlay.
#
# The lazy unmount detaches the old root tree (incl. the raw vda and scratch
# mounts) — the overlayfs holds its own references to the lower layer and
# upper dir, so this is safe and just hides the raw devices from the guest.
# `|| true`: a failed detach is cosmetic (vda is read-only at the VMM level);
# never panic the VM over it.
# NOTE: the detach also takes the devtmpfs mounted above with it, so /dev is
# an EMPTY directory from there until the per-tier init remounts it (both
# inits do this as their first act).
#
# /sbin/ix-init is the real init on the base/browser tiers and a symlink to
# /usr/local/bin/browser-vm-init on the browser-vm tier.
exec chroot . /bin/sh -c 'umount -l /old_root 2>/dev/null || true; exec /sbin/ix-init'
