# Small Non-Browser VM — Cross-Repo Integration Design

**Date:** 2026-05-29
**Status:** Draft
**Builds on:** `docs/superpowers/specs/2026-05-28-shared-browser-service-design.md` and its implementation plan `docs/superpowers/plans/2026-05-29-shared-browser-service.md`
**Repos:** `sandbox/ix` (go-sdk + rootfs), `oasis` (sandbox interface), `athena-new` (application wiring)

## Relationship to the existing shared-browser plan

The shared-browser **plan** already covers the ix-internal mechanism that makes small per-chat VMs possible:
- **Phase 1 (daemon-side `RemoteSharedBrowserBackend` + `IX_BROWSER_MODE`) is implemented** (`ix-core/config.rs` `BrowserMode`, `ix-browser/remote.rs`, wired in `ix-server/main.rs`). Verified present in the tree.
- **Phase 2 (Go Browser Gateway in `go-sdk`)** and **Phase 3 (browser-tier VM image + boot wiring)** are specced there as roadmap. That plan already **locks**: gateway in **Go inside `go-sdk`**, browser-tier VM lifecycle owned by the Go `IXManager`/VMM, placement keyed on the **pinchtab profile name** per chat_id. This document adopts those decisions verbatim — it does **not** re-spec the gateway or the browser-VM internals.

**What THIS document adds** (the piece the shared-browser plan does not cover): shrinking the per-chat VM itself — a small `base`-derived rootfs, lowered per-chat memory, and the cross-repo glue in **oasis** and **athena-new** to drive it. The shared-browser plan points per-chat VMs at the gateway; it never makes their image small. That is the user's actual goal here.

## Goal

Make per-chat / per-run sandbox VMs **small** for the common case (shell, code, file ops) by removing the browser stack from them, while keeping browser capability available on demand through a single **shared browser-tier VM**.

Today every sandbox boots a single heavy rootfs that includes the browser stack (Node + Chrome + Pinchtab, from the `browser`/`full` Docker stages); athena's fallback image default is `ghcr.io/nevindra/oasis/ix:latest`. A 30-run workload therefore replicates Chrome 30×. This design collapses that to N small VMs (no browser stack) + 1 shared browser tier.

This is the "browser-less light chat tier" that the shared-browser spec explicitly deferred as a separate ticket. The shared-browser daemon work (Phase 1: `RemoteSharedBrowserBackend` + `IX_BROWSER_MODE`) is **already merged**; this design completes the integration end-to-end across the three repos.

## Non-goals

- Rewriting pinchtab or the `BrowserBackend` trait (reused as-is).
- Multi-host or pool-of-N browser tiers (the shared-browser spec's Option 3 — forward-compatible, not built here).
- Hostile multi-tenancy hardening.
- Touching the `full` (scientific-Python) image beyond making it optional per sandbox.

## Current State (verified)

| Layer | Today |
|---|---|
| ix Dockerfile | `base` (Python+ixd) → `browser` (+Node/Chrome/Pinchtab) → `browser-vm` (pinchtab PID1, no ixd) / `full` (+sci-Python). `browser-vm` stage + `browser-vm-init.sh` exist. |
| ix go-sdk | `go build ./...` and `go test .` are **green**. Single `RootfsImage`; default per-sandbox **1 vCPU / 512 MB**. **The gateway library is DONE & tested but UNWIRED:** `gateway.go` (`Gateway`: full `/v1/browser/*` routing, per-chat pinchtab instance+tab lifecycle, egress check on navigate, heartbeat state machine, `DELETE /chats/{id}`, `/metrics`, per-VM in-flight cap) + `gateway_vsock.go` (`NewGatewayForBrowserVM(vsockUDS,token,maxInflight,logger)`) + `gateway_egress.go`+test (Go port of Rust egress, mirrored tests) all exist and pass. `buildEnvSlice` injects `IX_BROWSER_MODE=remote=<url>`+`IX_CHAT_ID` per chat (tested, `gateway_wiring_test.go`). **MISSING (the actual gaps):** nothing instantiates/serves the `Gateway` or boots a browser-tier VM — `NewManager`/`Create`/`Close` never reference it; no HTTP listener on a guest-reachable addr; no second-drive (state dir) attach in `vmm.go` (`startVMCold` attaches rootfs only); no browser-tier config knobs (only `BrowserGatewayURL string`); no per-chat "light" opt-out (buildEnvSlice sets remote mode whenever `BrowserGatewayURL != ""`); no LRU instance reaper. `build-rootfs-ext4.sh` supports `base`/`browser`/`full` tiers, **not `browser-vm`** (and that tier needs a different init: PID1 = `browser-vm-init`, no `ixd`). |
| ix daemon | Reads `IX_BROWSER_MODE` (`local` default, or `remote=<url>`), `IX_CHAT_ID`, `IX_BROWSER_GATEWAY_TOKEN`. `RemoteSharedBrowserBackend` implemented + wired. Daemon side (shared-browser Phase 1) is complete. |
| oasis | `sandbox.Sandbox` (browser methods always present) + `sandbox.Manager`. `CreateOpts{SessionID, Image, TTL, Resources{CPU,Memory,Disk}, Env}`. `sandbox.Tools(sb, ...)` always includes 7 browser tools. `WorkspaceInfoResult.Browser bool` reported at runtime. No ix import (interface injection). |
| athena-new | `ix/go-sdk` + `oasis`. One sandbox per run/tree (`SessionID=RunID`, TTL 1h). All agents get the full toolset incl. browser. Manager configured from `SANDBOX_*` env. |

## Target Architecture

```
athena-new (host process)
  └── ix.NewManager(cfg)                         ┌─────────────────────────────┐
        ├── per-chat VMs  (small, base image) ──▶│ in-process Browser Gateway  │
        │     ixd: shell/code/files             │ (goroutine http.Server on    │
        │     IX_BROWSER_MODE=remote=<gw>  ─────▶│  169.254.0.1:9100, passt-    │
        │     (or browser disabled = light)     │  reachable from guests)      │
        │                                        │  place(chat_id)→browser VM   │
        └── 1 browser-tier VM (browser-vm img) ◀─┤  egress check, heartbeat     │
              pinchtab + Chrome, vsock           └─────────────────────────────┘
              IX_BROWSER_STATE_DIR (host-mounted)
```

**Key property:** every per-chat VM is small. Browsing is shared and time-multiplexed. A run that never browses costs nothing extra; the "light" flag additionally strips the capability for runs declared browser-free.

## Decisions

**Adopted from the shared-browser plan (already locked there, restated for context):**
- Gateway is **Go, inside `go-sdk`** (reuses the `CONNECT 1024` vsock transport + Firecracker lifecycle; no new Rust crate or separate process). Browser-tier VM and gateway are owned by `IXManager`.
- Placement keys the **pinchtab profile name** on chat_id; one Chrome instance per chat in the shared tier.

**New decisions in this document (the small-VM layer):**
1. **Two rootfs images, not one.** The per-chat default flips to a small `base`-derived rootfs (no Chrome/Node/Pinchtab); the browser-tier image is the separate `browser-vm` artifact. `build-rootfs-ext4.sh` already supports a `base` tier (builds from `ix:base`, bakes in the shared `ix-init.sh` + `ixd` + `repl.py`), so the small image needs **no script change** — `IX_ROOTFS_IMAGE=ix-base.ext4 ./build-rootfs-ext4.sh base`.
2. **Capability is a first-class `CreateOpts` field in oasis**, not just an env hack, so the app expresses intent (`full` browser / `light` no-browser) and the ix impl maps it to image + env. Backward compatible (zero value = current behavior).
3. **Per-chat default memory drops** from 512 MB to a base-image-appropriate default (proposed 256 MB), operator-overridable.

**Scope decisions (confirmed with user, 2026-05-29):**
4. **This plan is END-TO-END: small per-chat VM + the connecting pieces (gateway + browser-tier VM).** Making per-chat VMs small is useless without somewhere for browser calls to go, so the gateway (shared-browser Phase 2) and the browser-tier VM boot/state-drive/vsock-bridge (Phase 3) are **in scope here**. This plan **absorbs and completes** Phases 2–3 of `docs/superpowers/plans/2026-05-29-shared-browser-service.md` (including their "must resolve first" items: guest vsock→pinchtab bridge, state persistence as a block device, browser-VM sizing) rather than re-speccing them from scratch.
5. **Browser-need signal is a per-agent config flag** in athena (`NeedsBrowser`/browser-enabled). Unknown/unset ⇒ browser-via-shared-tier (still a small VM). `false` ⇒ light small VM (browser disabled, browser tools omitted).

## Components & Changes

### A. ix — rootfs build

- **A1.** Build a small per-chat rootfs from the **`base`** Docker stage using the *existing* `build-rootfs-ext4.sh` (it already builds from `$IX_IMAGE` and installs the shared `ix-init.sh` + `repl.py`):
  ```bash
  docker build --target base -f daemon/cmd/Dockerfile -t ix-base:latest daemon/
  IX_IMAGE=ix-base:latest ./go-sdk/scripts/build-rootfs-ext4.sh ix-base.ext4 1024
  ```
  No script change required. (The browser-tier `ix-browser-vm.ext4` is produced by the shared-browser plan's Phase 3 from the `browser-vm` stage.)
- **A2.** Two artifacts result: `ix-base.ext4` (per-chat, target ≤ ~1 GB) and `ix-browser-vm.ext4` (shared tier).

### B. ix — go-sdk

> **Ownership:** B2 (browser-tier lifecycle), B3 (gateway internals) are **owned by the shared-browser plan's Phase 2–3** and summarized here only for the parts the small-VM layer depends on — do not re-spec them. B4 (per-chat remote env plumbing) is **already implemented** (`BrowserGatewayURL` → `IX_BROWSER_MODE`/`IX_CHAT_ID` in `buildEnvSlice`). The genuinely **new** work in this layer is **B1** (image/memory config knobs) and **B5** (WorkspaceInfo flag), plus the oasis (C) and athena (D) glue.

- **B1. `ManagerConfig` knobs — IN SCOPE for this layer:**
  - **Per-sandbox image override:** ensure `CreateOpts.Image` (oasis) flows through `Create`/`resolveOpts` so a sandbox can pick the small base image vs another; the manager default `RootfsImage` becomes the small base image. (Confirm the existing `resolveOpts` image path covers this; add wiring if not.)
  - **Per-chat default memory** lowered from 512 MB to **256 MB** (operator-overridable via `ManagerConfig.PerSandbox` / `CreateOpts.Resources`). 512 MB was sized for the heavy image; the base image + Python REPL fits in less.
  - *(Deferred to the shared-browser plan, listed for context only:* `BrowserTierImage`, `BrowserTierMemoryMB`, `BrowserStateDir`, `GatewayAddr`, `GatewayToken` — these belong to gateway/tier lifecycle, not this layer. `BrowserGatewayURL` already exists.)*
- **B2. Browser-tier lifecycle:** when `BrowserMode=remote`, `IXManager` on startup launches one browser-tier VM (`BrowserTierImage`, `BrowserTierMemoryMB`, state dir mounted) and starts the in-process gateway goroutine. Health-monitors it (reuse existing monitor pattern); restarts on failure. Shuts down on `Manager.Shutdown`.
- **B3. Gateway (new file, e.g. `go-sdk/browser_gateway.go`):**
  - `http.Server` bound to `GatewayAddr` (passt-reachable from guests).
  - Reads `X-IX-Chat-Id`, `X-IX-Egress-Policy`, optional `Authorization`.
  - `place(chatID) → browser-tier VM` (Option 2: the single tier VM). Maps `chat_id → pinchtab profile` via `?profile=<chat_id>` so each chat gets its own Chrome instance/profile.
  - Egress check on navigate-like calls before forwarding (parse the policy header; reuse `EgressPolicy` matching).
  - Forwards to the browser-tier VM over the **existing vsock transport**.
  - Heartbeat loop polls the tier VM `/health`; returns 503 while unhealthy.
  - `DELETE /chats/{chat_id}` for eager profile cleanup on run end.
  - Concurrency caps per spec's Resource Model (per-chat in-flight = 1; per-VM cap = `mem/250`; LRU reap when over).
- **B4. Per-chat remote plumbing — ALREADY IMPLEMENTED.** `ManagerConfig.BrowserGatewayURL` exists (`manager.go:45`) and `buildEnvSlice` (`manager.go:443-446`) already appends `IX_BROWSER_MODE=remote=<url>` + `IX_CHAT_ID=<chatID>` to each per-chat VM's env when it is set, with coverage in `gateway_wiring_test.go`. Remaining gap: also thread `IX_BROWSER_GATEWAY_TOKEN` through if/when auth is enabled. No other work here.
- **B5. `WorkspaceInfo` `browser` flag:** in remote mode the per-chat daemon should report `browser=true` (capability present via gateway); in light mode report `false`. The daemon already derives this from the backend's `available()`; confirm `RemoteSharedBrowserBackend.available()` returns true when a gateway URL is configured.

### C. oasis — sandbox interface

- **C1. `CreateOpts.Browser`** — add an optional tri-state capability. Recommend `Browser *bool` (nil = manager default; `true` = ensure browser; `false` = light/no-browser). Keeps the interface impl-agnostic; the ix `Manager` maps: `false` → base image + browser disabled; `true`/nil → base image + `IX_BROWSER_MODE=remote` (browser via shared tier). (No behavior change for callers that don't set it, once the manager default is chosen by the app.)
- **C2. `sandbox.Tools` option** — add `WithoutBrowser()` (or `Tools` consulting a passed capability) so the app can omit the 7 browser tools for light sandboxes. Alternatively, the app filters by `WorkspaceInfo().Browser` at wire time. Pick the explicit option for determinism.
- **C3. Docs:** note that browser tools route to a shared tier transparently when the impl is in remote mode; `WorkspaceInfo().Browser` reflects effective availability.

### D. athena-new — application wiring

- **D1. Manager config (`main.go`):** new env →
  - `SANDBOX_ROOTFS` keeps pointing at the **small base** rootfs (per-chat default).
  - `SANDBOX_BROWSER_MODE` (`local`|`remote`, default `remote` once tier image is provided).
  - `SANDBOX_BROWSER_ROOTFS` (browser-tier image), `SANDBOX_BROWSER_MEMORY_MB`, `SANDBOX_BROWSER_STATE_DIR`, `SANDBOX_GATEWAY_ADDR`, `SANDBOX_GATEWAY_TOKEN`.
  - Map these into the new `ix.ManagerConfig` fields (B1).
- **D2. Run/chat sandbox creation (`internal/adapter/oasis.go`):** keep `CreateOpts{SessionID, TTL}` but small VM is now the default. Set `Browser` from a **per-agent config flag** (`NeedsBrowser`, confirmed decision):
  - Add `NeedsBrowser *bool` (or equivalent) to the agent config persisted in athena. Unknown/unset ⇒ browser-via-shared-tier (small VM, `CreateOpts.Browser=nil`).
  - When the agent's flag is `false`, pass `CreateOpts.Browser=&false` and build tools with `sandbox.Tools(sb, sandbox.WithoutBrowser())` so the 7 browser tools are omitted.
  - Plumb the flag from agent config → `adapter.Request` → `CreateOpts`.
- **D3. Tooling:** `lazy_sandbox.go` unchanged (lazy create still works; browser methods now proxy to the tier).

## Data Flow — browser call from a small per-chat VM

```
1. agent → ixd (per-chat VM): POST /v1/browser/navigate {url}
2. RemoteSharedBrowserBackend → POST http://169.254.0.1:9100/v1/browser/navigate
     headers: X-IX-Chat-Id=<run_id>, X-IX-Egress-Policy=<json>, Authorization
3. go-sdk gateway: egress-check url → place(run_id)=tier VM →
     forward over vsock → POST <tier>/v1/browser/navigate?profile=<run_id>
4. pinchtab: profile→Chrome (launch on first use), execute, return
5. gateway → ixd → agent
```

Non-browser call path is unchanged and fully local to the small VM.

## Memory Model (target)

- Per-chat small VM: ~256 MB RAM, ~0.6 GB image (base). 30 chats ≈ 7.5 GB RAM for the compute tier.
- Browser tier: 1 VM @ 4 GB, Chrome reaped to keep concurrent instances ≪ chat count.
- Total at 30 chats ≈ within the shared-browser spec's ≤ 8 GB browser-tier budget plus a much lighter compute tier than today's ~21 GB.

## Failure Handling

Inherits the shared-browser spec's table. Additions:
- Browser-tier VM down ⇒ gateway 503 ⇒ per-chat backend surfaces `BrowserUnavailable`. Non-browser work on small VMs is unaffected.
- Light sandboxes: browser tools absent, so no failure surface.
- Gateway is in-process with the manager; if the manager process dies, all sandboxes die anyway (same blast radius as today).

## Testing Strategy

- **go-sdk unit:** gateway routing/placement, egress enforcement, heartbeat state machine, header parsing (mock tier server).
- **go-sdk integration (serial):** boot a small per-chat VM + a browser-tier VM; end-to-end navigate/screenshot/snapshot through the gateway; assert non-browser ops never touch the tier.
- **Resilience:** kill tier VM mid-test → 503 → recovery; cookies survive restart (state dir).
- **Memory:** 10/30/50 simulated chats; record per-chat RSS (assert small) + tier RSS (assert within budget).
- **oasis unit:** `CreateOpts.Browser` mapping; `Tools(WithoutBrowser())` omits 7 browser tools; `WorkspaceInfo().Browser` reflects mode.
- **athena:** run a non-browser task on a light sandbox (no browser tools offered); run a browsing task on a default small sandbox (routes to tier).

## Implementation Outline (for writing-plans) — END-TO-END

In dependency order. Two rootfs images, then go-sdk (small VMs + gateway + browser-tier boot), then oasis + athena, then validation.

**ix — rootfs**
1. **Light image:** build `ix-base.ext4` from the `base` tier with the existing `build-rootfs-ext4.sh` (no script change); document artifact + size. Verify a base VM boots and serves shell/code/files.
2. **Browser-tier image:** build `ix-browser-vm.ext4` from the `browser-vm` stage; resolve the **guest vsock→pinchtab bridge** (socat `VSOCK-LISTEN:1024 → TCP 127.0.0.1:9867`) and the init script (`browser-vm-init.sh`).

**ix — go-sdk (this is the bulk)**
3. **Config knobs (B1):** per-sandbox image override + default memory 512 MB → 256 MB; tests.
4. **Gateway (B3, new `go-sdk/browser_gateway.go`):** HTTP server on `GatewayAddr`; parse `X-IX-Chat-Id`/`X-IX-Egress-Policy`; `place(chat_id)→profile`; egress check; forward to tier VM over vsock; heartbeat → 503; `DELETE /chats/{id}`. Unit-test vs a mock pinchtab.
5. **Browser-tier lifecycle (B2):** `IXManager` boots the tier VM on startup (browser image, larger mem, state block-device, health-poll instead of READY), monitors/restarts, shuts down on `Shutdown`.
6. **WorkspaceInfo flag (B5):** `browser=true` in remote mode, `false` in light; test. (B4 per-chat env plumbing already exists.)

**oasis**
7. **C1/C2:** `CreateOpts.Browser *bool` + `sandbox.Tools(..., WithoutBrowser())`; map in the ix `Manager`; tests for tool omission + capability mapping.

**athena**
8. **D1/D2:** small base rootfs as default via `SANDBOX_ROOTFS`; `SANDBOX_BROWSER_MODE=remote` + browser-tier/gateway env; per-agent `NeedsBrowser` flag → `adapter.Request` → `CreateOpts.Browser` + `WithoutBrowser()`.

**validation**
9. **Integration (serial):** small VM + gateway + tier VM → end-to-end navigate/screenshot/snapshot; assert non-browser ops never touch the tier. Resilience: kill tier mid-test → 503 → recover, cookies survive.
10. **Memory benchmark:** 10/30/50 runs; per-chat RSS (small) + tier RSS (within budget).

## Open Questions (with current default)

1. **Per-chat default memory** — 256 MB proposed; validate the base image + Python REPL fits under load. *Default: 256 MB, operator-overridable.* (Resolve during implementation via the memory benchmark.)
2. ~~`NeedsBrowser` source~~ — **RESOLVED:** per-agent config flag; unknown ⇒ browser-via-tier (small VM).
3. ~~Tier dependency~~ — **RESOLVED:** end-to-end; gateway + browser-tier VM are in scope (absorbs shared-browser Phases 2–3).
4. **Gateway auth** — owned by the shared-browser plan (Phase 2). This layer just threads `IX_BROWSER_GATEWAY_TOKEN` if/when enabled.
