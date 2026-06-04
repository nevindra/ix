# Browser Wait Tool + Interaction Verification — Design

> Status: design approved 2026-06-03; **revised 2026-06-04** after source-level
> verification against the local pinchtab checkout
> (`/home/nezhifi/Development/sandbox/pinchtab`). The revision replaces the
> eval-polling mechanism with pinchtab's native `POST /wait` endpoint — see
> "Revision note". Implementation plan:
> `docs/superpowers/plans/2026-06-04-browser-wait-interaction-tools.md`.
> Spans four repos: `oasis` (sandbox pkg), `ix/daemon`, `ix/go-sdk`, `athena-new`.

## Problem

Pinchtab exposes ~34 MCP tools; the ix → oasis → athena chain surfaces only a
subset. A gap analysis (2026-06-03, against <https://pinchtab.com/docs/index-2/>)
found:

- **Interactions are already wired.** The oasis `browser` tool
  (`oasis/sandbox/tools.go:545`) exposes
  `navigate, click, type, fill, scroll, key, hover, press, select, focus` via
  its `action` enum. `navigate` dispatches to `BrowserNavigate`; everything
  else flows through the daemon's generic `POST /action` passthrough
  (`ix-browser/src/pinchtab.rs:291`) to Pinchtab's `/action`.
- **Wait utilities are absent at every layer.** Agents today have no way to
  wait for a condition; they guess with screenshots or fixed shell sleeps.

This design adds a first-class `browser_wait` capability, fixes two bugs found
during verification, and adds an end-to-end interaction sweep. It deliberately
does **not** chase the rest of the Pinchtab surface (tabs, network, dialog,
solve, cookies, profiles) — see Out of Scope.

## Revision note (2026-06-04)

The approved design assumed the daemon would implement waiting itself by
polling `/evaluate` (docs-based: pinchtab's HTTP bridge documented no wait
endpoint). Reading the actual pinchtab source falsified that and two other
assumptions:

1. **Pinchtab has a native `POST /wait`** (`internal/routes/routes.go:79`,
   also tab-scoped `POST /tabs/{id}/wait`, capability `CapNone`). Its handler
   (`internal/handlers/wait.go`) supports selector/text/notText/url/load/fn/ms
   modes with **default 10 s, cap 30 s, 250 ms poll** — identical numbers to
   the approved design — plus **true network-idle** detection. On timeout it
   returns **200 `{waited:false, elapsed, error}`**, which is exactly our
   "structured result, never errors" semantics. → The daemon now *proxies*
   `/wait` instead of reimplementing polling.
2. **Eval-polling would have been broken in local mode.** Pinchtab gates
   `/evaluate` (and wait's `fn` mode) behind `security.allowEvaluate`, default
   **false** (`internal/handlers/evaluate.go:20`). ix's local-mode
   `PINCHTAB_CONFIG` (`pinchtab.rs:19`) does not set it — meaning the
   **existing `browser_eval` tool 403s in local mode today** (latent bug).
   The browser-VM tier already sets it (`daemon/cmd/browser-vm-init.sh:96`).
   → This design adds `"allowEvaluate":true` to the local config.
3. **There is no ix daemon inside the browser-tier VM.** The gateway
   (`go-sdk/gateway.go`) translates `/v1/browser/<op>` straight to pinchtab's
   `/tabs/{tabId}/<op>` via a fixed per-op whitelist (`browserOps`,
   gateway.go:166). → Remote mode forwards the translated pinchtab body to the
   gateway, and the gateway needs one new whitelist entry: `"wait"`.
4. **The `key`-action mismatch is confirmed from source.** Pinchtab's action
   registry (`internal/bridge/action_registry.go`) registers `press` but not
   `key`; oasis's `browser` tool emits `key`. `ExecuteAction` returns
   `unknown action: key`. → Fixed by translating `key`→`press` in the daemon's
   action route.

## Decisions (made with user 2026-06-03; mechanism revised 2026-06-04)

| Decision | Choice |
|---|---|
| Agent-facing shape | **One unified `browser_wait` tool** with a `kind` enum — mirrors the existing `browser` action-enum pattern |
| Timeout semantics | **Structured result, never an error.** Timeout ⇒ `satisfied: false`; the agent branches (snapshot / retry / give up) |
| Kinds in scope | **All six**: `selector`, `text`, `url`, `load`, `time`, `function` |
| Daemon mechanism | **Proxy pinchtab's native `POST /wait`** (revised — was eval-polling). Verified against local pinchtab source |
| Interface evolution | **Grow the oasis `Sandbox` interface** (compile-time enforcement for every wrapper) rather than an optional capability interface, which would let a forgetful wrapper silently drop the tool |

## Architecture

```
oasis/sandbox        Sandbox interface + BrowserWaitOpts/Result + browser_wait tool
      │   (breaking: every Sandbox impl adds one method)
      ▼
ix/go-sdk            IXSandbox.BrowserWait → POST /v1/browser/wait (per-chat daemon)
      ▼                  + Gateway browserOps gains "wait" (remote tier)
ix/daemon            ix-core types · ix-browser trait wait() per backend ·
      │              ix-server route POST /v1/browser/wait
      ▼
pinchtab             POST /wait (local) · POST /tabs/{id}/wait (via gateway)
```

`athena-new` needs no tool wiring: `sandbox.Tools(sharedSandbox, ...)`
(`internal/adapter/oasis.go:634`) returns the new tool automatically, and
`WithoutBrowser()` excludes it for `NeedsBrowser=false` requests. Athena's only
change is the interface-growth ripple (one passthrough on `lazySandbox`).

## Agent-facing interface (oasis)

```go
type browserWaitArgs struct {
    Kind      string `json:"kind" describe:"What to wait for: selector, text, url, load, time, function"`
    Value     string `json:"value,omitempty" describe:"CSS selector (selector), text to appear (text), URL glob (url), load state ready-state|content-loaded|network-idle (load), JS expression (function). Unused for time."`
    TimeoutMs int    `json:"timeout_ms,omitempty" describe:"Max wait in ms (default 10000, max 30000). For kind=time this is the delay itself."`
    State     string `json:"state,omitempty" describe:"selector only: visible (default) or hidden"`
}
```

```go
// Added to the Sandbox interface:
BrowserWait(ctx context.Context, opts BrowserWaitOpts) (BrowserWaitResult, error)

type BrowserWaitOpts  struct { Kind, Value string; TimeoutMs int; State string }
type BrowserWaitResult struct { Satisfied bool; Kind string; ElapsedMs int; Detail string }
```

Tool description steers the agent: wait after navigate/click instead of
polling with screenshots; `satisfied=false` on timeout (never errors) — then
snapshot. Result rendering is plain text:
`condition met (selector) after 840ms` /
`condition NOT met (selector) after 10000ms: <detail>. Take a snapshot…`.

**Interface ripple** — every `Sandbox` implementation gains the method:
oasis's own `lazy.go`, the `mockSandbox` fakes in `tools_test.go` and
`lazy_test.go`, `ix/go-sdk` `IXSandbox`, athena's `lazySandbox`. All repos use
local `replace` directives, so no version dance during development.

## Daemon design

**Types** (`ix-core/src/types.rs`, snake_case wire format):
`BrowserWaitOpts { kind, value?, timeout_ms?, state? }`,
`BrowserWaitResult { satisfied, kind, elapsed_ms, detail? }`.

**Translation module** (`ix-browser/src/wait.rs`) — pure functions, mirrors the
`build_snapshot_path` testability pattern:

- `effective_timeout_ms(opts)` — default 10 000, capped 30 000 (pinchtab parity).
- `build_wait_body(opts) -> Result<serde_json::Value>` — maps ix kinds to
  pinchtab's `waitRequest` fields; validates here so local and remote enforce
  identical rules (`Error::BadRequest` → 400):

| ix kind | pinchtab body | notes |
|---|---|---|
| `selector` | `{selector, state}` | `value` required; `state` visible (default) \| hidden |
| `text` | `{text}` | `value` required |
| `url` | `{url}` | `value` required; pinchtab does a **glob** match |
| `load` | `{load}` | `value` optional: ready-state (default) \| content-loaded \| network-idle |
| `function` | `{fn}` | `value` required; needs `security.allowEvaluate` upstream |
| `time` | `{ms: timeout}` | the timeout **is** the delay |

  Every body carries `{"timeout": effective_timeout_ms}`.
- `PinchtabWaitResponse { waited, elapsed, error? }` +
  `to_wait_result(kind, resp)` → `{satisfied: waited, elapsed_ms: elapsed, detail: error}`.

**Trait** (`ix-browser/src/backend.rs`):
`async fn wait(&self, opts: BrowserWaitOpts) -> Result<BrowserWaitResult>;`

| Backend | Implementation |
|---|---|
| `PinchtabBackend` | `POST {base}/wait` with translated body |
| `RemoteSharedBrowserBackend` | `POST {gateway}/v1/browser/wait` with the **same translated body** (gateway relays it verbatim to `/tabs/{id}/wait`) |
| `NoopBrowserBackend` | `Error::Unavailable` |
| `MockBrowser` | configurable `wait_response` field |

Both real backends set a **per-request reqwest timeout** of
`effective_timeout_ms + 5 s`, because both clients are built with a global 30 s
timeout (`pinchtab.rs:31`, `remote.rs:33`) that would otherwise fire exactly at
a 30 s wait deadline.

**Route**: `POST /v1/browser/wait` in `ix-server/routes/browser.rs` +
`router.rs`, guarded by the existing `check_available` (503 when no browser).

**Config fix**: `PINCHTAB_CONFIG` (`pinchtab.rs:19`) gains
`"allowEvaluate":true` in `security` — fixes the existing local-mode
`browser_eval` 403 and enables `kind=function`. (browser-vm-init.sh already
sets it for the remote tier.)

**Interaction fix**: the action route (`routes/browser.rs:45`) translates
`kind:"key"` → `"press"` before dispatch, for both backends. The oasis enum is
left unchanged so existing agents/prompts keep working.

## go-sdk design

- `IXSandbox.BrowserWait(ctx, opts)` — `POST /v1/browser/wait` with the
  ix-format body `{kind, value, timeout_ms, state}`. Sandbox HTTP clients use
  2–5 min timeouts (manager.go), so no override is needed host-side.
- Gateway: add `"wait": {method: http.MethodPost}` to `browserOps`
  (gateway.go:166). The generic `makeBrowserHandler` then forwards body and
  response verbatim; the upstream vsock client already has a 60 s timeout
  (gateway_vsock.go:21). A 30 s wait holds the per-chat in-flight mutex — fine,
  the chat's agent is blocked on the wait anyway.

## athena-new design

One passthrough on `lazySandbox` (`internal/adapter/lazy_sandbox.go`), same
pattern as `BrowserAction` (line 163) — no retry wrapper, matching the other
non-idempotent browser ops:

```go
func (l *lazySandbox) BrowserWait(ctx context.Context, opts sandbox.BrowserWaitOpts) (sandbox.BrowserWaitResult, error) {
    sb, err := l.get(ctx)
    if err != nil {
        return sandbox.BrowserWaitResult{}, err
    }
    return sb.BrowserWait(ctx, opts)
}
```

## Error handling summary

| Condition | Behavior |
|---|---|
| Browser unavailable / disabled | 503 (existing `check_available`) |
| Missing `value` for selector/text/url/function | 400 (`build_wait_body`) |
| Unknown `kind` / invalid `state` | 400 (`build_wait_body`) |
| `timeout_ms` > 30 000 | clamp to 30 000 (not an error) |
| `kind=function` with evaluate disabled upstream | pinchtab 403 → `Error::Forbidden` |
| Condition not met by deadline | **200** with `satisfied: false` + `detail` |

## Interaction verification (second scope item)

The `key`→`press` mismatch is already confirmed and fixed from source (above).
Remaining assurance is a **gated e2e sweep**: `ix-browser/tests/browser_e2e.rs`
(`#[ignore]`, needs pinchtab + Chrome in PATH, run serially) drives a local
known-elements page and exercises every post-translation action kind —
`click, type, fill, scroll, press, hover, select, focus` — plus
wait smoke tests (selector/text/load + timeout path).

## Testing

| Layer | Tests |
|---|---|
| ix-browser (Rust) | `wait.rs` units: timeout default/clamp, body per kind (+validation errors), response mapping; mock/noop trait coverage |
| ix-browser remote | wiremock: translated body forwarded, timeout-as-success-false, validation short-circuits without HTTP |
| ix-server | router tests: wait 503 when unavailable, wait 200 structured result, action `key`→`press` translation |
| go-sdk | gateway test: `/v1/browser/wait` → `POST /tabs/{id}/wait`; sandbox httptest round-trip |
| oasis | tool dispatch + timeout rendering; WithoutBrowser excludes `browser_wait` |
| E2E (gated) | real pinchtab+Chrome: 8-kind interaction sweep + wait smoke |

## Risk

The Docker image pulls `pinchtab/pinchtab:latest` and the browser-VM rootfs
bundles its own pinchtab build. `/wait` exists in the local source the gateway
was verified against; an **older deployed binary without `/wait`** would
return 404 → `Error::NotFound("pinchtab: …")`. The gated e2e suite catches
this against whatever binary is in PATH.

## Out of scope (recorded follow-ups)

- `tabId` / multi-tab, network inspection, dialog handling, solve/AutoSolver,
  cookies, profiles, handoff.
- Enriching `navigate` with pinchtab's `waitFor`/`waitSelector`/`timeout` body
  params — good next increment.
- Exposing pinchtab's `notText` wait mode and `idleFor` tuning knob.
