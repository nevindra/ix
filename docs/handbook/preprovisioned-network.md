<!-- source-of-truth: go-sdk/scripts/ix-host-setup.sh, go-sdk/netmanifest.go, go-sdk/netprovider.go, go-sdk/manager.go -->

# Preconfigured (rootless) network mode

> **Who should read this:** Operators who want to run the ix `IXManager` as an
> unprivileged service account — no `sudo`, no `CAP_NET_ADMIN` at runtime.
>
> **What you'll learn:** Why root is normally needed, how preconfigured mode
> moves all of that work to a one-time setup step, how to run that step, and
> how to configure the manager to use it.

---

## Why root is normally required

In the default mode, `IXManager` creates a TAP device and installs nftables NAT
rules for each VM at the moment it boots. Both operations require
`CAP_NET_ADMIN` (effectively root). This is fine for a developer workstation but
undesirable for a production service that should run as a locked-down account.

Preconfigured mode separates the work:

- **Once, as root:** `ix-host-setup.sh` provisions all TAP devices, installs the
  `ix-nat` nftables table, enables IP forwarding, and writes a manifest
  describing the pre-built network topology.
- **At runtime, as an unprivileged user:** the manager reads the manifest and
  *attaches* to an already-owned TAP — which requires no elevated privilege
  because the TAP is already owned by the service account.

---

## Quickstart

```bash
# once, as root
sudo bash go-sdk/scripts/ix-host-setup.sh --taps 32 --user ixsvc

# then, as ixsvc (no sudo, no caps)
IX_PRECONFIGURED_NETWORK=1 ./your-service
```

---

## 1. One-time host setup (as root)

```bash
sudo bash go-sdk/scripts/ix-host-setup.sh \
  --taps 32 \
  --user ixsvc
```

The script is **idempotent** — safe to re-run if you need to change the tap
count or other settings. Re-running tears down and recreates the TAP devices and
rewrites the manifest.

### Flags

| Flag | Default | Description |
|---|---|---|
| `--taps N` | `32` | Number of TAP slots to pre-provision. This is the hard concurrency cap — the manager cannot boot more VMs simultaneously than there are TAPs. |
| `--user USER` | *(required)* | The unprivileged service account that will own the TAPs and run the manager. Must already exist on the host. |
| `--egress-iface IF` | *(none)* | Pin NAT masquerade to a specific uplink interface. If omitted, masquerades on any non-TAP interface — more robust on multi-homed or VPN hosts. |
| `--gateway-ip IP` | *(none)* | Pin this IP on the dummy interface `ixgw0`. Required only when using the remote browser tier. |
| `--manifest PATH` | `/etc/ix/network.json` | Where to write the network manifest. |

### What the script does

1. Enables `net.ipv4.ip_forward` immediately and persists it via
   `/etc/sysctl.d/90-ix.conf` (survives reboot).
2. Installs the `ix-nat` nftables table with a POSTROUTING masquerade rule and
   FORWARD accept rules for `ixtap*` interfaces.
3. If Docker is present, inserts ACCEPT rules into `DOCKER-USER` (or `FORWARD`)
   so Docker's default DROP policy doesn't block VM traffic.
4. If `--gateway-ip` is given, creates the dummy interface `ixgw0` and pins the
   IP to it.
5. Creates `ixtap0` … `ixtapN-1` as persistent TUN/TAP devices owned by
   `--user`, assigns host IPs, and brings them up.
6. Writes the network manifest to `--manifest`.

---

## 2. Running the manager (as the unprivileged user)

Set the environment variable before starting your service:

```bash
IX_PRECONFIGURED_NETWORK=1 ./your-service
```

Or set it in Go before constructing the manager:

```go
cfg := ix.ManagerConfig{
    PreconfiguredNetwork: true,
    // NetworkManifest defaults to /etc/ix/network.json
}
```

`IX_PRECONFIGURED_NETWORK=1` is read by `applyDefaults` and is equivalent to
setting `ManagerConfig.PreconfiguredNetwork = true`. The manifest path defaults
to `/etc/ix/network.json` and can be overridden with `ManagerConfig.NetworkManifest`.

No `sudo`, no elevated capabilities. The manager will fail fast at startup with
an actionable error if the setup step was not run correctly (see
[Failure modes](#4-failure-modes) below).

---

## 3. Manifest schema

`ix-host-setup.sh` writes a JSON file (default `/etc/ix/network.json`) that
describes the pre-provisioned network topology. The manager reads this file at
startup.

```json
{
  "version": 1,
  "cidr": "172.16.0.0/16",
  "egress_iface": "",
  "owner": "ixsvc",
  "gateway_ip": "",
  "taps": [
    {
      "idx": 0,
      "name": "ixtap0",
      "host_ip": "172.16.0.1",
      "guest_ip": "172.16.0.2",
      "guest_mac": "06:00:AC:10:00:02",
      "mask": "255.255.255.252"
    }
  ]
}
```

**Fields:**

- `version` — schema version; the manager rejects anything other than `1`.
- `cidr` — the subnet used for guest addressing (currently always
  `172.16.0.0/16`).
- `egress_iface` — the uplink pinned at setup time, or empty if defaulting to
  any non-TAP interface.
- `owner` — the service account that owns the TAPs.
- `gateway_ip` — the IP pinned on `ixgw0` for the remote browser tier, or
  empty if not configured.
- `taps` — one entry per pre-provisioned TAP. Addressing follows a deterministic
  `/30` scheme: slot `n` → host IP `172.16.(4n/256).(4n%256)+1`, guest IP
  `+2`. The MAC is derived from the guest IP.

**The tap pool is the hard concurrency cap.** The manager clamps `MaxConcurrent`
down to the number of entries in `taps` at startup. To raise concurrency, re-run
`ix-host-setup.sh` with a larger `--taps` value.

---

## 4. Failure modes

The manager performs all checks at startup and fails fast with an actionable
error. Nothing starts if any of these conditions hold.

| Condition | Error | Fix |
|---|---|---|
| Manifest missing or unreadable | `failed to load network manifest … run ix-host-setup` | Run `sudo bash go-sdk/scripts/ix-host-setup.sh --taps N --user USER` |
| `net.ipv4.ip_forward` is `0` | `ip_forward is disabled … run sudo ix-host-setup` | Re-run host setup; the script enables and persists it |
| Manifest version ≠ 1 | `network manifest version X unsupported (want 1); re-run ix-host-setup` | Re-run host setup to regenerate with the current schema |
| Manifest has zero TAPs | `network manifest has no taps; re-run ix-host-setup` | Re-run host setup with `--taps N` where N ≥ 1 |
| Manifest CIDR ≠ `ManagerConfig.NetworkCIDR` | config mismatch error | Align `ManagerConfig.NetworkCIDR` with the manifest, or re-run setup |
| Pool exhausted at `Create` time | returns `ErrNetworkPoolExhausted` | Queue or surface to callers; re-run setup with more `--taps` to raise the limit |
| Preconfigured + remote browser, no `gateway_ip` in manifest | fails at manager init | Re-run host setup with `--gateway-ip IP` |

---

## 5. Limits (this iteration)

- **Custom CIDR is not yet supported.** The subnet is always `172.16.0.0/16`.
  The `--cidr` flag does not exist yet. `--taps` is capped at 16384 (the /16
  carved into /30s).

- **Run `ix-host-setup` *after* Docker is installed/started.** The script picks
  the `DOCKER-USER` vs `FORWARD` chain at setup time. If Docker is installed or
  restarted afterward, the FORWARD-accept rule can land in the wrong chain and
  VM egress will be silently dropped — in preconfigured mode the manager does
  not re-apply it at runtime. Re-run `ix-host-setup` after any Docker change.

- **TAPs and nftables rules do not survive reboot.** `ip_forward` persists (via
  `/etc/sysctl.d/90-ix.conf`), but TAP devices and nft rules are lost on reboot.
  Re-run `ix-host-setup.sh` after each reboot, or wrap it in a systemd oneshot
  unit that runs after `network.target`:

  ```ini
  [Unit]
  Description=ix network pre-provisioning
  After=network.target
  Before=your-service.service

  [Service]
  Type=oneshot
  ExecStart=/usr/local/bin/ix-host-setup --taps 32 --user ixsvc
  RemainAfterExit=yes

  [Install]
  WantedBy=multi-user.target
  ```

- **Multiple managers on one host are not supported.** TAP names (`ixtap0` …)
  and `ixgw0` are global. Running two manager instances simultaneously will
  cause TAP conflicts.

- **`RunDir` must be on a real filesystem owned by the service user.** It cannot
  be on `tmpfs`. It defaults to a directory adjacent to `RootfsImage`; ensure
  the service account has write access.
