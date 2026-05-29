# ix Shared Browser Service — Design

**Date:** 2026-05-28
**Status:** Draft
**Goal:** Decouple browser capability from the per-chat sandbox lifecycle so that many chats share a small number of Chrome processes — ship as a single shared browser-tier VM today, with a forward-compatible interface that scales to a pool of browser-tier VMs without daemon-side rework.

## Context

Today every chat in ix maps 1:1 to a Firecracker MicroVM, and each VM independently runs pinchtab + headless Chrome. A 30-chat workload therefore spawns 30 sandbox VMs, 30 pinchtab processes, and 30 Chrome processes — roughly 21 GB of RAM, most of it consumed by per-chat Chrome instances.

The browser is the heaviest sub-capability of the sandbox. Shell, code execution, and file ops are cheap and naturally per-chat (private workspace). Browsing is heavy *and* mostly stateless between chats — there is no good reason for it to be replicated per chat.

This design lifts the browser out of the per-chat VM into a shared browser-tier VM, while preserving:

- Today's `BrowserBackend` trait on the daemon side (drop-in replacement via a new backend impl).
- Pinchtab as the in-VM browser engine (we reuse, not rebuild, its profile/instance management, ref caching, and snapshot logic).
- Firecracker as the isolation boundary (Chrome still runs inside a VM, not on the host).

## Goals

- **Memory:** at 30 active chats, total RAM for the browser tier ≤ 8 GB (vs. ~21 GB today). At 100 chats, ≤ 22 GB. This is contingent on idle chats' Chrome instances being reaped so concurrent Chrome count stays well below total chats: sharing pinchtab does not by itself reduce per-chat Chrome (it is still ~1 Chrome per *active* chat) — the savings come from collapsing per-VM (Firecracker + ixd) overhead plus time-multiplexing of bursty browsing.
- **Latency:** a browser call from a per-chat VM completes within +20 ms of today's local-pinchtab latency (one extra network hop through the gateway).
- **Isolation:** between-chat soft isolation via pinchtab's per-profile Chrome instances + profile dirs. Chrome's blast radius remains confined to the browser-tier VM (kernel boundary preserved).
- **Forward-compatible:** moving from one browser-tier VM to a pool of N is config + scheduler changes only — no edits to the per-chat daemon, the `RemoteSharedBrowserBackend`, or the browser-tier VM image.
- **Operator opt-in:** existing deployments that prefer in-VM browsers keep working via the existing `PinchtabBackend`. The new `RemoteSharedBrowserBackend` is selected by config.

## Non-goals

- Hostile multi-tenancy. The trust model is "different end-users with Chrome BrowserContext / per-agent profile isolation acceptable" plus "one user, many chats may share state." Hardening to truly mutually-distrusting tenants is out of scope.
- Replacing pinchtab. We use it as-is (`internal/orchestrator/orchestrator.go:357`) and treat its per-profile Chrome instances (`Orchestrator.Launch`, keyed by profile name) as our placement primitive.
- Multi-host scale-out. Single-host pool is the upgrade ceiling for this spec. Multi-host comes later.
- Reactive context resurrection. The first release crashes a chat's open tabs if the browser-tier VM restarts; storage_state persistence keeps cookies/logins but not live page state.

## Approach Summary

Build Option 2 (one shared browser-tier VM) with Option 3 (pool) as a no-refactor upgrade. The five seams that make this possible:

1. A host-resident **Browser Gateway** that all per-chat daemons call.
2. A **chat_id-keyed** call protocol — pinchtab's **profile name** is the placement key.
3. A **host-mounted state directory** so `storage_state` survives browser-VM restart and (later) supports cross-VM migration.
4. **Per-chat egress policy carried at the request layer**, applied inside the browser-tier VM.
5. **Heartbeat/health from each browser-VM to the gateway**, even when there is only one upstream.

In Option 2, the gateway's placement function is a one-liner. In Option 3, the same function becomes a real load-aware router. Everything else is unchanged.

## Architecture

```
Per-chat VM (×N)                          Host                       Browser-tier VM
┌──────────────────┐               ┌──────────────────────┐         ┌──────────────────────┐
│  ixd daemon      │  HTTP over    │  Browser Gateway     │  HTTP   │  pinchtab server     │
│  ───────────     │  passt        │  - chat_id → profile │  over   │  - multi-instance    │
│  ix-browser:     │  (outbound    │  - egress dispatch   │  vsock  │  - profile-keyed     │
│  Backend trait   │   from guest) │  - heartbeat poll    │ ──────▶ │  - 1 Chrome/profile  │
│   = RemoteShared │ ────────────▶ │  - placement func    │         │    (1..K processes)  │
└──────────────────┘               └──────────────────────┘         └──────────────────────┘
                                            │                                 │
                                            └──── shared state dir ───────────┘
                                                  /var/lib/ix/browser-state/
                                                  (pinchtab profile dirs, host-mounted)
```

**Networking detail.** Per-chat VMs reach the gateway over their existing passt outbound (the host listens on a well-known port, e.g. `169.254.0.1:9100`, which passt makes reachable from guests). The gateway reaches the browser-tier VM the same way the Go SDK reaches per-chat daemons today: vsock (host→guest is cheap), dial the browser-VM's CID on port 1024 with the same `CONNECT` handshake (`vsock transport` section of CLAUDE.md). No new transport code — we reuse the daemon's vsock-listener pattern, point it at pinchtab, and let the gateway speak the same protocol from the host side.

### What's new vs. today

| Component | Status |
|---|---|
| `ix-browser::BrowserBackend` trait | Already exists (`daemon/crates/ix-browser/src/backend.rs:8`). Unchanged. |
| `PinchtabBackend` impl (in-VM) | Already exists. Unchanged. Operator-selectable. |
| `RemoteSharedBrowserBackend` impl | **New.** HTTP client to the gateway, carries chat_id. |
| Browser Gateway daemon | **New.** Small host-side process. Rust, sharing `ix-core` types. |
| Browser-tier VM image | **New.** Reuses the `browser` Dockerfile stage; pinchtab is PID 1. No ixd. |
| `IX_BROWSER_MODE` env / config flag | **New.** `local` (default) or `remote=<gateway-url>`. |
| Per-chat egress propagation | **Modified.** ixd surfaces its egress policy as request headers when calling out. |
| Host state dir mount | **New.** `/var/lib/ix/browser-state/` mounted into browser-tier VM. |

## Components

### `RemoteSharedBrowserBackend` (daemon, `daemon/crates/ix-browser/`)

A new struct alongside `PinchtabBackend`. Implements the same `BrowserBackend` trait, so route handlers in `ix-server` are unchanged.

Responsibilities:

- Hold the gateway endpoint URL and the chat's `session_id` (which becomes `chat_id` for routing).
- For each browser method (`navigate`, `screenshot`, `action`, `snapshot`, etc.), translate the call to an HTTP request against the gateway. Pass `X-IX-Chat-Id`, `X-IX-Egress-Policy` (JSON), and the chat's authorization token in headers.
- Stream SSE responses through transparently — the daemon already uses SSE for shell/code; we keep that pattern for any streaming browser endpoint.
- Surface gateway errors as the same `ix_core::Error` variants that `PinchtabBackend` raises, so route-level error mapping is identical.

Selection: `DaemonConfig` reads `IX_BROWSER_MODE`. When it's `remote=<url>`, `ix-server` instantiates `RemoteSharedBrowserBackend` in `State`. Otherwise the existing `PinchtabBackend` is used.

### Browser Gateway (host, new crate `ix-browser-gateway/`)

A small Rust HTTP server running on the host (not inside any VM). Sits behind a single bind address reachable from per-chat VMs via passt (their existing user-mode network).

Responsibilities:

- **Routing.** Receive browser calls with `X-IX-Chat-Id`. Apply `place(chat_id) → vm_id`. In Option 2, `place` returns the only browser-VM. In Option 3, `place` becomes a real scheduler (least-loaded sticky-after-first-use).
- **Translation.** Rewrite the inbound URL to the upstream pinchtab's URL on the chosen browser-VM. Map the chat_id to a pinchtab **profile name** so the orchestrator launches (or reuses) one Chrome instance per chat, keyed by profile.
- **Egress enforcement seam.** Read the `X-IX-Egress-Policy` header. For Option 2 the gateway enforces this *before* forwarding (DNS allowlist check on navigate-like requests). This keeps egress logic out of pinchtab.
- **Heartbeat.** Background loop polls `GET /health` on each known browser-VM every 10 s. If a VM goes unhealthy, gateway returns 503 to incoming calls for chats placed there (Option 2: all calls; Option 3: only that VM's chats).
- **Observability.** Per-chat call counters + last-seen, exposed at `/metrics`. Used both for ops and (later) for the placement decision.

### Browser-tier VM image (new Docker stage / rootfs)

Reuses the existing `browser` stage in `daemon/cmd/Dockerfile` (Node.js + Chrome + Pinchtab) but **drops `ixd`** — this VM does not need shell/code/files. PID 1 is pinchtab in server mode.

Boot args carry:

- `IX_BROWSER_STATE_DIR=/var/lib/ix/browser-state` — the host-mounted directory pinchtab writes profile dirs into.
- Pinchtab session idle/lifetime config plus `instanceDefaults.tabPolicy.lifecycle=close_idle` and `tabEvictionPolicy=close_lru` — caps live tabs per instance.
- Pinchtab has no built-in per-profile Chrome reaper, so capping live Chrome instances per VM is gateway-side logic we add (it stops idle chats' instances via `Orchestrator`; see Resource Model).

The browser-tier VM gets a larger memory budget than a sandbox VM (rough day-1 target: 4 GB for ≤30 chats, scaling roughly linearly with chat count via launch-arg).

### State persistence (`/var/lib/ix/browser-state/`)

Pinchtab already stores per-instance profile dirs on disk. We host-mount that directory into the browser-tier VM. Two payoffs:

1. **Browser-VM restart survives logins.** When the VM comes back, pinchtab re-attaches to existing profile dirs and chats keep their cookies / localStorage. Open tabs are lost; that's acceptable.
2. **Future cross-VM migration.** In Option 3, if a browser-VM dies, the gateway can re-place affected chats onto a survivor and the destination pinchtab finds the profile dir already present.

No new sync code is required for Option 2 — pinchtab's existing on-disk storage is sufficient. For Option 3 a small lockfile-per-chat will be added to prevent two browser-VMs claiming the same profile.

### Per-chat egress policy

Today: each per-chat VM's `ix-egress` enforces DNS-level allowlist for traffic originating in that VM. Once Chrome moves out of the per-chat VM, that firewall no longer covers browser traffic.

New flow:

1. The per-chat daemon already holds the chat's egress policy in `DaemonConfig`.
2. `RemoteSharedBrowserBackend` serializes the policy into the `X-IX-Egress-Policy` header on every outbound request.
3. The gateway parses it. For navigate-like calls, it checks the requested host against the allowlist/denylist before forwarding. Disallowed → respond `403` to the daemon without involving pinchtab.

Limitation: this enforces egress at navigation boundaries, not at every sub-resource fetch Chrome makes. For tighter coverage we'd need to set Chrome's `--host-resolver-rules` or run a proxy in the browser-tier VM. Day-1 enforcement-at-navigation is the bar; sub-resource control is deferred to a follow-up if needed.

### Heartbeat

Gateway pings each browser-VM's `GET /health` every 10 s with a 2 s timeout. State machine per upstream: `Healthy → Degraded (1 fail) → Unhealthy (3 fails) → Healthy (1 success)`. While `Unhealthy`, the gateway returns `503 Service Unavailable` to incoming browser calls bound to that VM. The daemon's `RemoteSharedBrowserBackend` surfaces this as `ix_core::Error::BrowserUnavailable`, which routes treat the same as today's pinchtab-down case.

## Data Flow — Single Browser Call

```
1. User → ix-server in per-chat VM:   POST /v1/browser/navigate {"url": "..."}
2. ix-server → BrowserBackend trait method navigate(url)
3. RemoteSharedBrowserBackend builds:
     POST <gateway>/v1/browser/navigate
     headers: X-IX-Chat-Id, X-IX-Egress-Policy, Authorization
     body:    {"url": "..."}
4. Gateway:
     a. Resolve chat_id → vm_id  (Option 2: trivial)
     b. Egress check on url
     c. Forward → POST <vm-pinchtab>/v1/browser/navigate?profile=<chat_id>
5. Pinchtab on browser-VM:
     a. Resolve profile → Chrome instance (launch on first use, reuse if running)
     b. Execute navigate against that Chrome
     c. Return result
6. Gateway → daemon → ix-server → user
```

Latency budget: native pinchtab call (~30 ms) + passt out-of-guest (~3–5 ms) + gateway forward + vsock into browser-VM (~3 ms) ≈ +6 to +10 ms over today's local-pinchtab path. Well within the +20 ms target.

## Failure Handling

| Failure | Behavior |
|---|---|
| Gateway crashes | Per-chat daemons receive connection errors → surface as `BrowserUnavailable` until host process supervisor restarts gateway. |
| Browser-tier VM crashes | Heartbeat flips to `Unhealthy`; gateway returns 503 for affected chats. On restart, pinchtab re-discovers profile dirs; cookies/logins persist; open tabs lost. |
| Idle Chrome instance reaped | Chat's next browser call relaunches the Chrome instance for the same profile. Profile dir already on disk → cookies preserved. Tabs reset. Transparent to the daemon. |
| Per-chat egress violation | Gateway returns 403; daemon surfaces `ix_core::Error::EgressDenied`. Same error code as today's in-VM egress firewall. |
| Stale chat_id (chat ended) | The gateway stops the profile's Chrome instance via `Orchestrator`; pinchtab's session idle timeout is the backstop. We add a `DELETE /chats/{chat_id}` gateway endpoint that the per-chat VM calls on chat close, for eager cleanup. |

## Resource Model

Pinchtab does not cap the number of live Chrome instances on its own, so the gateway bounds concurrent profiles per browser-VM. Gateway adds:

- **Per-chat concurrency limit:** at most 1 in-flight browser call per chat (pinchtab's scheduler also enforces `maxPerAgentInflight`, default 10).
- **Per-VM in-flight limit:** N × concurrency, rejected with 429 above that.
- **Max live Chrome instances per browser-VM:** chosen at launch from the VM memory arg (`memory_mb / 250` as a starting heuristic); the gateway reaps the least-recently-used chat's instance via `Orchestrator` when over the cap.

## Pool Upgrade Path (Option 3)

What changes when moving from one browser-VM to N:

| Layer | Change required |
|---|---|
| Per-chat daemon (`RemoteSharedBrowserBackend`) | **None.** Still calls the gateway. |
| Browser-VM image | **None.** Identical artifact, just more instances. |
| Pinchtab inside the VM | **None.** Profile/instance logic untouched. |
| Browser Gateway — `place(chat_id)` | Replace the one-liner with least-loaded + sticky-after-placement (`HashMap<chat_id, vm_id>` + per-VM load counters). |
| Browser Gateway — upstream discovery | Read `IX_BROWSER_UPSTREAMS` (comma-separated URLs) instead of `IX_BROWSER_UPSTREAM`. |
| State directory | Add a per-chat lockfile so two browser-VMs can't both adopt the same profile. |
| Heartbeat | Loop over all upstreams (same code, longer iteration). |

No daemon recompile. No per-chat VM image change. Operator gains horizontal scale by deploying additional browser-VMs and pointing the gateway at them.

## Testing Strategy

Match the existing daemon test conventions (per-crate unit tests + serial integration tests).

- **Unit:** `RemoteSharedBrowserBackend` against a mock gateway HTTP server. Assert all `BrowserBackend` methods translate correctly and surface the right error variants.
- **Unit:** Gateway placement, heartbeat state machine, egress enforcement, header parsing.
- **Integration (serial):** Spin up gateway + a real browser-tier VM image + one per-chat VM. End-to-end navigate / screenshot / snapshot via the daemon. Use the existing `cargo test -p ix-server -- --test-threads=1` pattern.
- **Resilience:** Kill the browser-tier VM mid-test; assert gateway flips to 503 and recovers on restart. Verify cookies survive across restart.
- **Memory:** Benchmark with 10 / 30 / 50 simulated chats; record total RSS of browser-tier VM. Pass criterion: stays within the budget set at VM launch.

## Open Questions

1. **Gateway language.** Rust (share `ix-core` types, single ecosystem) vs. Go (closer to the existing `go-sdk` pool/health code). Recommend Rust for type sharing with the daemon.
2. **Egress sub-resource enforcement.** Day-1 ships navigation-level enforcement only. Decide whether sub-resource enforcement via Chrome host-resolver-rules is needed before GA.
3. **Auth between daemon and gateway.** Day-1 assumes the gateway bind address is only reachable from per-chat VMs via passt. Should we add a shared secret (`IX_BROWSER_GATEWAY_TOKEN`) anyway?
4. **Browser-VM sizing heuristic.** Auto-tune from host RAM (similar to `IXManager`'s concurrency auto-detect) or require explicit operator config?
5. **Snapshot interaction.** ix has a snapshot/restore path for sandbox VMs. Does the browser-tier VM also get a golden snapshot for fast cold start, or is its boot cost amortized across many chats and therefore negligible?

## Out of Scope

- Cross-host scale-out (load-balanced gateways).
- True hostile multi-tenancy hardening (Chrome process-per-trust-domain, seccomp profiles, gVisor).
- Sub-resource-level egress.
- Live page-state migration across browser-VM restarts.
- Browser-less ("light") chat tier (separate ticket).
