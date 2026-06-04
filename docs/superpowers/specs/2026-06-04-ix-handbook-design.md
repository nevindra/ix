# ix Handbook — Design

**Date:** 2026-06-04
**Status:** Approved
**Output:** `docs/handbook/` — 6 markdown files

## Goal

A documentation set that explains ix in detail — architecture, the browser
subsystem, integration, and operations — readable by people with average
coding skills (internal team onboarding + technically-curious stakeholders
who "vibe code"). English. Lives in the ix repo so it versions with the code.

## Audience & reading paths

| Reader | Path |
|---|---|
| Stakeholder / PM | `01-what-is-ix.md` only |
| New engineer | 01 → 02 → 03 → 04 |
| Operator / deployer | 01 → 05 |
| App developer integrating ix | 04 (+ 03 if using browser) |

## Writing principles (apply to every document)

- English; short sentences; no unexplained jargon — every term (vsock,
  rootfs, pool, …) gets a one-line plain-language explanation or analogy at
  first use, then is used normally.
- Mermaid diagrams for every major concept (GitHub renders them). Sequence
  diagrams for request flows, flowcharts for decisions.
- Code only as short, real, copy-pasteable snippets — no long listings.
- Every document opens with "Who should read this & what you'll learn" and
  closes with "Where to go next".
- Source of truth is the current code (branch `feat/shared-browser-remote-backend`),
  including the shared browser tier and the new `browser_wait` tool.
- Each file carries `<!-- source-of-truth: <code paths> -->` HTML comments so
  future edits know what to re-check when code changes.

## Documents

### `README.md` — index (~half page)
Table: document → who it's for → questions it answers. The reading paths above.

### `01-what-is-ix.md` — The Big Picture (~3-4 pages, no code)
1. The problem — AI agents run untrusted code/browsing; your laptop/server is
   not the place for it.
2. The solution — "a hotel with disposable rooms": each chat gets its own tiny
   computer (Firecracker MicroVM); checkout = the room is destroyed.
3. Why MicroVMs, not Docker — isolation vs speed comparison table.
4. What a sandbox can do — shell, code, files, fetch/search, browser, egress
   firewall — two sentences each.
5. The cast of characters — one-screen diagram: app → manager → VM → daemon
   (+ optional browser tier).
6. Performance & cost intuition — benchmark numbers from README, explained.

### `02-architecture.md` — How It Works (~6-8 pages)
1. Two halves — Go SDK (host) vs Rust daemon (guest); golden rule: "every
   sandbox operation is an HTTP request".
2. Anatomy of one request — `sb.Shell("echo hi")` hop by hop (sequence
   diagram); SSE vs plain JSON.
3. The daemon up close — crate map with one-line description per crate;
   env-var-only config; transport priority vsock → unix → tcp.
4. The manager up close — pool (why create is near-instant), monitor/reaper
   goroutines, TTL, golden snapshots ("emulator save-state" analogy).
5. Networking & isolation — three layers: vsock (API), TAP+NAT (outbound
   internet), ix-egress (DNS firewall); diagram.
6. Security model — what a sandbox can NOT do.

### `03-browser.md` — The Browser Subsystem (~5-6 pages)
1. Why browsers are special — Chrome ≈700 MB; the 30-chats × 700 MB math.
2. One interface, two modes — `BrowserBackend`; decision flowchart on
   `BrowserGatewayURL`.
3. Mode 1: in-VM — sequence diagram; when to use.
4. Mode 2: shared tier — gateway, browser-tier VM, pinchtab server mode,
   profile-per-chat, egress enforced at the gateway; sequence diagram.
5. The toolbox — table of all browser tools (navigate, screenshot, snapshot,
   action, eval, find, wait) + when an agent uses which; `browser_wait`
   detail: six kinds, timeout is NOT an error.
6. Quirks & gotchas — key→press translation, allowEvaluate, pinchtab version
   vs `/wait`.

### `04-integration.md` — Plugging ix Into Your App (~5-6 pages, agnostic)
1. The contract — oasis's `sandbox.Sandbox` interface is the single door;
   anything that speaks this interface can use ix.
2. The three-layer cake — diagram: your app → oasis (interface + tools) →
   ix go-sdk (implementation) → VM; one-line responsibility per layer.
3. Minimal integration — ~30 lines: NewManager → Create → `sandbox.Tools(sb)`
   → hand to any agent loop.
4. Tool auto-registration — `Tools()` returns ~20 ready tools; options like
   `WithoutBrowser`.
5. Lifecycle patterns — eager vs lazy create (why lazy: VM only created on
   first tool call), session mapping, cleanup.
6. Case study: athena — lazySandbox wrapper, per-op retry policy; written as
   "one way to apply the patterns", not a requirement.
7. Integrating WITHOUT oasis — call the daemon REST API directly (table of
   main endpoints) for non-Go apps.

### `05-operations.md` — Running ix (~5-6 pages)
1. What you need — KVM, firecracker, vmlinux kernel, rootfs; checklist.
2. Building blocks — 4-stage Dockerfile → tier table (base/browser/full/
   browser-vm) → `build-rootfs-ext4.sh`; **"code change X = rebuild what"
   matrix** (ixd → every tier's ext4 + golden snapshots; gateway/oasis/athena
   → host binary only; browser-vm only when pinchtab/init script changes).
3. Deploying the shared browser tier — order: browser-vm.ext4 → gateway →
   `BrowserGatewayURL`.
4. Golden snapshots — when to regenerate.
5. Day-2 operations — health checks, TTL/reaping, disk pressure, restart
   circuit-breaker.
6. Troubleshooting table — symptom → likely cause → what to check (404 on
   wait = old ixd; 403 on eval = old config; pinchtab NotFound = old binary;
   …).

## Out of scope (deliberate)

- Full per-endpoint API reference (code + tests serve as reference).
- Pinchtab internals.
- libkrun migration (still design-stage).
- Benchmark methodology.

## Sources of truth per document

| Doc | Read before writing |
|---|---|
| 01 | `README.md`, `CLAUDE.md` |
| 02 | `README.md`, `daemon/crates/*` structure, `go-sdk/manager.go`, `go-sdk/sandbox.go`, `go-sdk/vmm*.go`, networking spec in `docs/superpowers/specs/` |
| 03 | `daemon/crates/ix-browser/src/*`, `go-sdk/gateway.go`, `daemon/cmd/browser-vm-init.sh`, oasis `sandbox/tools.go`, `docs/superpowers/specs/2026-05-28-shared-browser-service-design.md`, `docs/superpowers/specs/2026-06-03-browser-wait-interaction-tools-design.md` |
| 04 | oasis `sandbox/{sandbox,tools,lazy}.go`, `go-sdk/{manager,sandbox}.go`, athena `internal/adapter/{lazy_sandbox,oasis}.go`, daemon `router.rs` |
| 05 | `go-sdk/scripts/*.sh`, `daemon/cmd/Dockerfile`, `daemon/cmd/browser-vm-init.sh`, `README.md`, browser-tier specs |
