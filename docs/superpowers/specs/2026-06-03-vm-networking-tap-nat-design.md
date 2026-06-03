# VM Networking — per-VM TAP + NAT (replace orphaned passt)

> Status: design approved 2026-06-03; gateway-reachability addendum approved
> 2026-06-03. Implementation plan to follow.

## Problem

`go-sdk` Firecracker VMs currently have **no external network**. `startVMCold`
starts `passt` (`network.go`) but **never wires it to Firecracker** — there is no
`PUT /network-interfaces` anywhere in the codebase. The VMs get only vsock +
(down) loopback.

Root cause: **passt is the wrong tool for Firecracker.** passt is a QEMU
socket / vhost-user backend (`--socket`, `--vhost-user`, `--fd` all connect to
the VMM via a stream protocol). **Firecracker's network is TAP-only**
(`PUT /network-interfaces` with `host_dev_name` + `guest_mac`); it does not
support vhost-user-net. `pasta` does not fit either (it owns taps inside a
netns; FC needs its own host tap). So the existing passt code can never provide
connectivity and must be **replaced**, not extended.

Impact: Chrome in the browser tier can boot and serve `/health` but cannot
navigate to real sites; `ix-fetch` / web-search in any sandbox cannot reach the
internet.

## Goal

Give every Firecracker VM (per-chat base VMs and the shared browser tier)
outbound IPv4 connectivity via the canonical Firecracker approach: a **per-VM
TAP device + kernel `ip=` autoconfiguration + a one-time host NAT
(MASQUERADE) + IP forwarding**. Sandbox VMs remain isolated from each other
(per-VM `/30`); the existing daemon-side DNS egress firewall continues to apply
on top.

Non-goals: IPv6, inter-VM connectivity, multi-queue, port-forwarding into VMs.

## Privilege model (decided)

**The manager self-manages taps + NAT, holding `CAP_NET_ADMIN`.** Tap
create/destroy happens in-process (no per-VM `sudo`); the pre-warm pool hides
tap-creation latency off the VM-boot critical path. Operators grant the
capability via `setcap cap_net_admin+ep <ixd-manager-binary>` or systemd
`AmbientCapabilities=CAP_NET_ADMIN`, or run privileged. (The tap logic is
isolated so it can later move into a small setuid helper for stricter prod
separation without changing call sites.)

## Approach

Per-VM TAP + kernel `ip=` autoconfig + one-time host NAT. The orphaned `passt`
code is removed.

### Addressing

- Address space: `172.16.0.0/16`, carved into `/30` subnets, one per VM.
- Index `n` (free-list allocator — `alloc()` **reuses freed indices**, unlike the
  existing monotonic `allocateCID` counter, which never recycles) → subnet base
  `172.16.0.0 + n*4`: host/gateway `= base+1`, guest `= base+2`, mask
  `255.255.255.252`. (`n ∈ [0, 16383]` ⇒ 16384 concurrent VMs.)
- TAP name: `ixtap<n>`.
- `guest_mac = 06:00:<guest-ip-as-4-hex-octets>` (matches the Firecracker docs
  convention, e.g. guest `172.16.0.2` → `06:00:AC:10:00:02`). Deterministic and
  unique because guest IPs are unique.

### Components

**`network.go` (rewrite — replaces passt):**

- `tapAllocator` — atomic free-list of indices, `alloc() (int, error)` /
  `free(int)`; reuses indices from torn-down VMs.
- `type vmNet struct { idx int; tapName, hostIP, guestIP, guestMAC, mask string }`
- `setupTap(n int) (vmNet, error)` — derive fields from `n`; run
  `ip tuntap add <name> mode tap`, `ip addr add <hostIP>/30 dev <name>`,
  `ip link set <name> up`. (Process holds `CAP_NET_ADMIN`; no `sudo`.)
- `teardownTap(net vmNet)` — `ip link del <name>`; `alloc` index freed by caller.
- `ensureHostNAT(egressIface string) error` — **idempotent, once at manager
  start**: `sysctl net.ipv4.ip_forward=1`; create `nft` table `ip ix-nat` with a
  `postrouting`/`srcnat` chain (MASQUERADE for `saddr 172.16.0.0/16
  oifname <egress>`) and a forward-accept rule for tap traffic. Re-runnable
  (flush chain, re-add rule) so no duplicate rules. (`nft` v1.1.6 confirmed on
  the host; auto-detected egress iface on this host = `enp6s0`.)
- `ensureForwardAccept() error` — **idempotent, once at manager start**, right
  after `ensureHostNAT`. Inserts an `iptables` ACCEPT for `ixtap+` traffic into
  `DOCKER-USER` (when that chain exists) or `FORWARD`. **Required because the nft
  forward-accept above is not sufficient on its own:** netfilter drops a packet
  if *any* base chain at the forward hook drops it, and Docker sets the iptables
  `FORWARD` policy to `DROP` — an nft `accept` cannot override an iptables
  `DROP` at the same hook. Best-effort: if `iptables` is absent it only warns
  (assumes no DROP policy). Verified on the dev host (Docker present, `-P FORWARD
  DROP`): without this, all VM egress times out even to raw IPs.
- `detectEgressInterface() (string, error)` — first `dev` of the default route
  (`ip route show default`), overridable via config.
- `ensureGatewayAddr(gatewayIP string) error` — **idempotent, once at manager
  start, only when `BrowserMode=="remote"`** (the shared browser Gateway is the
  sole host service guests dial by IP). Creates a host dummy interface `ixgw0`
  and assigns the Gateway's link-local IP (`169.254.0.1/32`, parsed from
  `GatewayListenAddr`) to it. See **Gateway reachability** below. Re-runnable
  (ignore "File exists"); torn down best-effort at manager stop
  (`ip link del ixgw0`). Needs `CAP_NET_ADMIN` (already held).

**`vmm.go`:**

- `VMMHandle`: replace `PasstPID int` with the `vmNet` (tap name + index).
- `startVMCold`:
  - replace `startPasst(...)` with `idx := tapAlloc.alloc(); net := setupTap(idx)`;
  - `PUT /network-interfaces` `{ iface_id:"eth0", guest_mac, host_dev_name:tapName }`
    (before `InstanceStart`);
  - append `ip=<guest>::<gw>:255.255.255.252::eth0:off:8.8.8.8` to the boot args
    (kernel configures `eth0` + DNS — **no guest iproute2 needed**);
  - on any error after tap setup: `teardownTap` + free index;
  - store `vmNet` in the handle.
- `cleanup`: `teardownTap(handle.vmNet)` instead of `killPID(handle.PasstPID)`.
- `buildKernelBootArgs(envSlice, net vmNet)` — gains the `ip=` argument
  (skipped when networking is disabled).

**Snapshot path — out of initial scope (cold-boot first).** Firecracker bakes
the network device (incl. `host_dev_name`, guest MAC) and the guest's `ip=`
addressing into the snapshot. Restoring N clones with per-VM `/30`s is hard: the
guest's gateway IP is baked, so every clone would need a tap whose host IP
matches the *same* baked gateway — which collides at host routing. FC's
`network_overrides` on `PUT /snapshot/load` can remap `host_dev_name` per clone,
but reconciling per-clone subnets needs a shared-gateway scheme (a bridge or a
fixed gateway IP). **Decision:** this spec implements networking for the
**cold-boot path only** (`startVMCold`), which covers the shared browser tier
(it already cold-boots — see `startBrowserTier`) and cold-booted per-chat VMs.
Snapshot-restored VMs keep today's behaviour (vsock-only) until a follow-up adds
a shared-gateway scheme + `network_overrides`. `startVM` only adds the tap on the
cold-boot branch; the snapshot-restore branch is unchanged. **Implication for the
browser-remote feature:** a snapshot-restored per-chat VM has no tap, so it can
reach neither the internet nor the host Gateway — run per-chat VMs from the
pre-warm pool (cold-boot) until the follow-up lands (see **Gateway
reachability**). **The golden boot (`CreateGolden`) is itself cold-booted, so it
must skip the tap** (`startVMCold(..., forceNoNet=true)`); otherwise it would
bake a `host_dev_name`/`ip=` into the snapshot that no longer exists at restore.
This keeps restored clones genuinely vsock-only.

**Guest:**

- Kernel `ip=` autoconfigures `eth0` and seeds DNS. `CONFIG_IP_PNP` is
  **confirmed present** in `/opt/ix/firecracker/vmlinux.bin` (`IP-Config:`
  strings), so no guest `iproute2` fallback is needed for IP config. Boot-test
  still confirms the guest device name is `eth0`.
- `browser-vm-init` / `ix-init`: write `/etc/resolv.conf` (`nameserver 8.8.8.8`)
  as a belt-and-suspenders for DNS (`ix-init` already does).

**Config (`ManagerConfig`):**

- `EgressInterface string` — host uplink for NAT; auto-detected if empty.
- `NetworkCIDR string` — base space; default `172.16.0.0/16`.
- `DisableNetworking bool` — escape hatch: skip tap setup (vsock-only VM).

### Gateway reachability (browser-remote)

The shared browser tier serves a Gateway on the host at a fixed link-local
address — `GatewayListenAddr`, default `169.254.0.1:9100` (`browser_tier.go`).
Per-chat VM daemons are told to reach the browser tier at exactly that URL
(`IX_BROWSER_MODE=remote=http://169.254.0.1:9100`, baked into `ix-core` config
and injected by `buildEnvSlice`). The per-VM `/30` scheme gives every VM a
*different* host-side gateway IP (`172.16.0.{4n+1}`), so nothing on the host
owns `169.254.0.1` — `net.Listen` on it would fail and guests could not route
to it. `ensureGatewayAddr` closes this gap by **pinning the fixed address on
the host** (chosen over a shared bridge, which would break per-VM isolation, and
over per-VM gateway URLs, which would force dynamic URL injection and change the
existing config flow):

- `ensureGatewayAddr` assigns `169.254.0.1/32` to a host dummy interface
  (`ixgw0`) at manager start, **before** `startBrowserTier` binds the Gateway
  (`manager.go:190`) — so the listen succeeds.
- Reachability (no forwarding/NAT — this is host-local delivery): a guest's
  packet to `169.254.0.1` is outside its `/30`, so it follows the default route
  to its host tap IP (`172.16.0.{4n+1}`); the host accepts the packet for its
  own local address `169.254.0.1` and delivers it to the Gateway socket; the
  reply (src `169.254.0.1`) routes back out the connected `/30` tap to the
  guest. `rp_filter` passes (the guest source reverse-routes to its own tap).
  No MASQUERADE applies; this composes with the outbound-internet path.
- The fixed `169.254.0.1:9100` URL is therefore **unchanged** everywhere
  (`ix-core` config, `manager.go` env injection, `browser-vm-init`).

**Boot mode for per-chat VMs (supported path).** This networking lands on the
cold-boot path (`startVMCold`), and the pre-warm pool keeps fully-booted,
fully-networked VMs ready so hand-out is effectively instant — the original
design's speed mechanism. The browser-remote feature is therefore supported for
**cold-booted / pooled** per-chat VMs. **Known limitation:** with
`UseSnapshot=on`, per-chat VMs restore from a golden snapshot and get **no tap**,
so they can reach neither the internet nor the Gateway until the snapshot
networking follow-up lands (see the snapshot section). Run per-chat VMs from the
pool (cold-boot) for the browser feature until then.

### Data flow

```
manager start → ensureHostNAT(egress)            [once: ip_forward + nft masquerade]
              → ensureGatewayAddr(169.254.0.1)    [once, browser-remote: ixgw0 dummy + /32]
              → startBrowserTier                  [binds Gateway on 169.254.0.1:9100]
startVMCold   → alloc idx → setupTap(idx)         [ip tuntap add/addr/up]
              → PUT /network-interfaces (tap, mac)
              → boot args += ip=guest::gw:mask::eth0:off:dns
              → InstanceStart
guest boot    → kernel configures eth0 from ip=   → app traffic
  outbound    → tap → host routing → nft MASQUERADE → egress iface → internet
  to gateway  → tap → host-local delivery (169.254.0.1 on ixgw0) → browser Gateway
teardown      → cleanup → ip link del tap → free idx
manager stop  → ip link del ixgw0                 [best-effort]
```

The daemon's DNS-level egress firewall (`ix-egress`) still runs inside the guest
and gates allow/deny; NAT only provides L3 reachability. They compose.

### Error handling

- `setupTap` failure → free index, fail VM creation (no partial tap leak).
- `ensureHostNAT` failure at startup → fail fast with a clear message
  (networking misconfigured) unless `DisableNetworking`.
- `teardownTap` is best-effort on cleanup (log, don't block shutdown); a leaked
  `ixtap<n>` is reclaimed because indices are freed and names are deterministic
  (a stale same-name tap is deleted before re-create).

### Testing

- **Unit (TDD):** `tapAllocator` alloc/free/reuse + exhaustion; IP/MAC/subnet
  derivation from `n` (table tests incl. octet rollover at `n=64`); `ip=`
  boot-arg builder; nft/`ip` command construction; `detectEgressInterface`
  parsing; `ensureGatewayAddr` command construction + gateway-IP parse from
  `GatewayListenAddr` + idempotency (re-run tolerates "File exists").
- **Boot-test (existing FC harness):** real VM boots, `eth0` comes up from `ip=`,
  guest reaches `https://example.com`; a cold-booted per-chat guest reaches the
  host Gateway at `169.254.0.1:9100`; browser tier `BrowserNavigate` succeeds
  end-to-end; teardown removes the tap.
- **Regression:** `go test -count=1 .` green; remove/replace `passt`-specific
  tests in `network_test.go`.

## Operator notes

- Grant `CAP_NET_ADMIN` to the manager. Run as **root** or, for a non-root
  systemd service, set `AmbientCapabilities=CAP_NET_ADMIN` (ambient caps
  propagate to the `ip`/`nft`/`iptables`/`sysctl` children). Plain
  `setcap cap_net_admin+ep` on the binary does **not** work — the cap is not in
  the inheritable/ambient set, so the shelled-out children get nothing.
- NAT + `ip_forward` are **host-wide** changes the manager applies at startup
  (idempotent). The `nft` table is named `ix-nat` for easy inspection/removal.
- **Docker (or any `-P FORWARD DROP`) on the host:** the manager adds an
  `iptables` ACCEPT for `ixtap+` to `DOCKER-USER`/`FORWARD` (see
  `ensureForwardAccept`). Without it the masquerade never runs and VM egress
  times out. The container/host needs the `iptables` binary on its `PATH`.
- `EgressInterface` override for hosts with non-obvious default routes.
- Browser-remote only: the manager pins the Gateway's link-local IP
  (`169.254.0.1`) on a host dummy interface `ixgw0` at startup (idempotent) so
  guests can reach the shared Gateway; removed on shutdown. Inspect with
  `ip addr show ixgw0`.
- The browser-remote feature requires per-chat VMs to **cold-boot** (pre-warm
  pool); it does not work with `UseSnapshot=on` until snapshot networking lands.
- **One manager owns the host's networking.** Tap names (`ixtap<n>`) and `ixgw0`
  are global; the allocator is per-manager. Running multiple managers on one
  host is **not supported** (they would collide on tap names and tear down each
  other's `ixgw0`). The host-wide `ix-nat` table and `ip_forward` are left in
  place on shutdown (idempotent); the `iptables` forward-accept rules and
  `ixgw0` are removed.
```
