# Preprovisioned Host Network Mode (rootless manager) — Design

**Date:** 2026-06-11
**Status:** Draft — specced from downstream integration experience; not yet planned

## Problem

`ix.NewManager` owns host networking and assumes root: `ensureHostNAT` runs
`sysctl -w net.ipv4.ip_forward=1` and `nft -f -` (network.go:239-249), and
every VM create/destroy shells out to `ip tuntap add/del`, `ip addr`,
`ip link` (network.go, tap setup). All of these are child processes, so file
capabilities on the embedding binary don't propagate — the only ways to run a
manager today are full root or sudo wrappers around the embedding process.

Real-world impact: a regular-user dev workflow embedding the manager fails at
the NAT step, and the platform silently degrades to no-sandbox mode.
Any production deployment of an ix-embedding service currently needs root,
which is a much larger privilege grant than the actual need (network setup is
static and one-time; only TAP attach is per-VM).

## Goal

A documented mode where **all privileged work happens once, as root, at host
setup time**, and the manager runs fully unprivileged afterwards:

```
sudo ix-host-setup --taps 32 --user ixsvc      # once per host / boot
...
IX_PRECONFIGURED_NETWORK=1 <service runs as ixsvc, no caps>
```

## Non-goals

- Rootless Firecracker beyond what already works (`/dev/kvm` via group, vsock
  UDS in a user-writable RunDir — both already non-root).
- User namespaces / slirp-style usermode networking (perf + complexity).
- Windows/macOS anything.

## Key insight

A **persistent TAP device created with an owner** (`ip tuntap add ixtap0 mode
tap user ixsvc`) can be opened and attached by that user without
CAP_NET_ADMIN — `TUNSETIFF` on an existing owned TAP is permitted. Everything
else the manager does at runtime (sysctl, nft, addr/link config) is static
per-host state that a setup script can do once, provided the per-VM address
plan is deterministic.

## Design

### 1. Host setup script `ix-host-setup` (new, ships in go-sdk/scripts/)

Run as root, idempotent:

- `sysctl -w net.ipv4.ip_forward=1` + persist to `/etc/sysctl.d/90-ix.conf`.
- Install the nft `ix-nat` table (same ruleset as `ensureHostNAT`, rendered
  for the configured CIDR + egress iface).
- Create a TAP pool: `ixtap0..ixtapN-1`, each
  `ip tuntap add $name mode tap user $USER`, `ip addr add <hostIP>/30 dev
  $name`, `ip link set $name up` — addresses follow the manager's existing
  deterministic per-index /30 scheme so the manager can map pool slot →
  subnet without any `ip` calls.
- Write a manifest `/etc/ix/network.json`: CIDR, egress iface, tap names +
  host IPs + guest IPs, owning user. The manager treats this as the source of
  truth.

### 2. Manager: preconfigured mode

Activated by `ManagerConfig.PreconfiguredNetwork: true` (or env
`IX_PRECONFIGURED_NETWORK=1` in the embedding service):

- Skip `ensureHostNAT` entirely. Optionally verify `ip_forward` is 1 by
  *reading* `/proc/sys/net/ipv4/ip_forward` (read needs no privilege) and fail
  fast with a "run ix-host-setup" error if not.
- TAP lifecycle becomes **allocate/release from the pool** instead of
  create/delete: pick a free entry from the manifest, attach Firecracker to
  it, mark busy; on VM teardown just release the slot (no `ip link del` —
  persistent TAPs stay). Concurrency cap = pool size; `Create` returns a
  typed error when the pool is exhausted.
- All `exec ip/sysctl/nft` paths gated behind `!PreconfiguredNetwork`.

### 3. Failure modes

- Manifest missing / unreadable → fail manager init with actionable error.
- TAP missing or owned by another user → fail that allocation with a
  "re-run ix-host-setup" error; other slots keep working.
- Pool exhausted → typed error, embedding service can queue or surface it.

### 4. Egress modes

`SANDBOX_EGRESS_MODE=allow` (gateway-enforced allowlists) is orthogonal —
egress enforcement lives in the gateway/vsock layer, not in host net setup —
but `ix-host-setup` must install the same nft base table that allowlist mode
expects, so both modes work unprivileged.

## Open questions (resolve during planning)

1. Browser-tier VMs use passt — confirm whether passt needs any privilege in
   this layout (it is designed to be rootless; likely fine).
2. Scratch disk provisioning (`RunDir` next to rootfs) — already user-owned,
   but `ix-host-setup` should chown the default `/opt/ix/rootfs/run`.
3. Pool sizing guidance vs `PoolSize`/`MaxConcurrent` in ManagerConfig —
   probably: manifest pool size is the hard host cap, ManagerConfig stays the
   soft cap.
4. Whether recovery (orphaned socket dir cleanup) touches anything privileged.

## Acceptance

- On a host prepared with `ix-host-setup`, the go-sdk integration suite
  (`go test -tags=integration ./...`) passes **as a regular user with no
  sudo** in preconfigured mode.
- Existing root-mode behavior unchanged when the flag is off.
- A regular-user dev workflow embedding the manager gets `sandbox enabled` with
  `IX_PRECONFIGURED_NETWORK=1` — replacing any interim sudoers-shim workaround.

## Sibling fixes spotted during the same integration push (separate, small)

While debugging a downstream rootfs integration, three latent issues surfaced
that belong to ix regardless of this feature:

1. `ix-init.sh` does not export `PATH` — kernel-spawned init has an empty
   environment, so in-VM `python3` resolves via `/bin` (usrmerge), computes
   `sys.prefix=/`, and silently drops `/usr/local/lib/pythonX/dist-packages`
   (every pip-installed package invisible). Fix: `export PATH=...` at the top
   of ix-init. (Already patched in a downstream vendored copy.)
2. `ix_repl.py` in the published rootfs path must match the daemon's
   `__IX_RESULT__` sentinel protocol — a stale repl makes every `exec` call
   hit the 120s SSE timeout.
3. GHCR `ix:latest` does not ship `/usr/local/bin/ixd`, but
   `build-rootfs-ext4.sh` consumers assume images do — either bake ixd into
   published images or make the script fail loudly (a downstream vendored copy
   now errors when ixd is absent and `IX_REPO` is unset).
4. Cosmetic: `pivot_root` leftovers are visible to in-VM globbing
   (`**/raw.csv` matches `./old_root/scratch/...`) — `umount -l /old_root` is
   lazy; consider a detached mount namespace or eager umount.
