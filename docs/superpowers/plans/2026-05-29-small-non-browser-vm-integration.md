# Small Non-Browser VM — Cross-Repo Integration Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Per the user's global git rule: do NOT `git commit` between tasks.** Stage files (`git add`) as noted, but leave the working tree dirty. The user commits in their own batches. (The TDD "Commit" steps below are therefore "stage" steps.)

**Goal:** Make per-chat / per-run sandbox VMs small (base image, no Chrome) for non-browser work, while keeping browser capability working through one shared browser-tier VM fronted by the (already-built) in-process Gateway.

**Architecture:** athena's `IXManager`, when `BrowserMode=remote`, boots one browser-tier Firecracker VM (the `browser-vm` rootfs: pinchtab + Chrome, no ixd) and serves the existing `Gateway` (`go-sdk/gateway.go`) on a guest-reachable address. Per-chat VMs boot the small `base` rootfs; `buildEnvSlice` points browser-enabled chats at the gateway (`IX_BROWSER_MODE=remote=<url>`) and marks browser-free chats `disabled`. oasis exposes a `CreateOpts.Browser *bool` capability and a `Tools(WithoutBrowser())` option; athena drives both from a per-agent `NeedsBrowser` flag.

**Tech Stack:** Go (`go-sdk` manager/vmm/gateway, `oasis/sandbox`, athena `internal/adapter`), Rust (daemon `ix-browser`/`ix-core`), bash (rootfs build), Firecracker + passt + vsock.

**Critical pre-existing state (verified, do NOT rebuild):**
- `go-sdk/gateway.go` — `Gateway` with full `/v1/browser/*` routing, per-chat pinchtab instance+tab lifecycle, egress check, heartbeat state machine, `DELETE /chats/{id}`, `/metrics`, in-flight cap. **Complete + tested.**
- `go-sdk/gateway_vsock.go` — `NewGatewayForBrowserVM(vsockUDS, token, maxInflight, logger) *Gateway`. **Complete.**
- `go-sdk/gateway_egress.go` (+`_test.go`) — Go port of the Rust egress matcher. **Complete + tested.**
- `go-sdk/manager.go:426` `buildEnvSlice` — already injects `IX_BROWSER_MODE=remote=<url>`+`IX_CHAT_ID` when `cfg.BrowserGatewayURL != ""`. **Tested** (`gateway_wiring_test.go`).
- `daemon/cmd/Dockerfile` `browser-vm` stage + `daemon/cmd/browser-vm-init.sh` — pinchtab server + `socat VSOCK-LISTEN:1024 → TCP 127.0.0.1:9867`. **Complete.**
- Daemon Phase 1 (`RemoteSharedBrowserBackend`, `BrowserMode::{Local,Remote}`, `Error::Forbidden`) — **merged.**
- Baseline: in `go-sdk/`, `go build ./...` and `go test -count=1 .` are **green**. Establish this before starting.

**The gaps this plan closes:** (1) nothing boots the browser-tier VM or serves the `Gateway`; (2) no second-drive (state dir) attach in `vmm.go`; (3) no browser-tier config knobs; (4) no per-chat "light" opt-out (a base-image chat with no browser must not try in-VM pinchtab); (5) per-chat default memory still 512 MB; (6) `build-rootfs-ext4.sh` has no `browser-vm` tier; (7) oasis has no browser-capability field / tool opt-out; (8) athena has no per-agent browser signal.

---

## File Structure

| File | Responsibility | Phase |
|---|---|---|
| `oasis/sandbox/manager.go` | Add `Browser *bool` to `CreateOpts` | A |
| `oasis/sandbox/tools.go` | Add `WithoutBrowser()` option; gate the 7 browser tools | A |
| `daemon/crates/ix-core/src/config.rs` | Add `BrowserMode::Disabled` + parse `IX_BROWSER_MODE=disabled` | B |
| `daemon/crates/ix-browser/src/noop.rs` (new) | `NoopBrowserBackend` (`available()=false`, all methods → `Unavailable`) | B |
| `daemon/crates/ix-browser/src/lib.rs` | Export `NoopBrowserBackend` | B |
| `daemon/crates/ix-server/src/main.rs` | Select `NoopBrowserBackend` for `Disabled` | B |
| `go-sdk/manager.go` | `ManagerConfig` browser-tier knobs; 256 MB default; light opt-out in `buildEnvSlice`; thread `CreateOpts.Browser` | C |
| `go-sdk/vmm.go` | `extraDrives` param on `startVMCold`; `/drives/state` attach helper | D |
| `go-sdk/browser_tier.go` (new) | Boot browser-tier VM, serve `Gateway`, lifecycle; called from `NewManager`/`Close` | E |
| `go-sdk/scripts/build-rootfs-ext4.sh` | Add `browser-vm` tier (PID1 = `browser-vm-init`, no ixd) | F |
| `athena-new/main.go` | Read `SANDBOX_BROWSER_*` → new `ManagerConfig` fields | G |
| `athena-new/internal/features/agents/model.go` | Add `NeedsBrowser *bool` | G |
| `athena-new/internal/adapter/oasis.go` | Map agent `NeedsBrowser` → `CreateOpts.Browser` + `WithoutBrowser()` | G |

**Dependency order:** A → C, A → G, C → E, D → E, B → C(light path). Phases A, B, D, F have no inbound deps and can start in parallel. Recommended serial order for a single worker: **A, B, D, F, C, E, G.**

---

# Phase A — oasis: browser capability + tool opt-out

Produces: an impl-agnostic way for callers to declare "no browser" and omit the 7 browser tools. Self-contained; testable with `go test ./sandbox/`.

### Task A1: Add `Browser *bool` to `CreateOpts`

**Files:**
- Modify: `oasis/sandbox/manager.go` (`CreateOpts`, lines 34–41)
- Test: `oasis/sandbox/manager_test.go` (create if absent)

- [ ] **Step 1: Write the failing test**

Add to `oasis/sandbox/manager_test.go`:

```go
package sandbox

import "testing"

func TestCreateOpts_BrowserField(t *testing.T) {
	yes := true
	no := false
	if (CreateOpts{}).Browser != nil {
		t.Fatal("zero-value CreateOpts.Browser should be nil (manager default)")
	}
	if got := (CreateOpts{Browser: &yes}).Browser; got == nil || *got != true {
		t.Fatalf("Browser=&true not preserved, got %v", got)
	}
	if got := (CreateOpts{Browser: &no}).Browser; got == nil || *got != false {
		t.Fatalf("Browser=&false not preserved, got %v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd oasis && go test ./sandbox/ -run TestCreateOpts_BrowserField 2>&1 | tail -10`
Expected: FAIL — `unknown field Browser in struct literal`.

- [ ] **Step 3: Add the field**

In `oasis/sandbox/manager.go`, extend `CreateOpts`:

```go
type CreateOpts struct {
	SessionID string            // conversation/session identifier (required)
	Image     string            // container image; empty uses manager default
	TTL       time.Duration     // sandbox lifetime; 0 uses manager default
	Resources ResourceSpec      // per-sandbox resource limits; zero values use defaults
	Env       map[string]string // additional env vars injected into the container
	// Browser declares whether this sandbox needs browser capability.
	// nil  = manager default (typically browser via shared tier);
	// true = ensure browser; false = no browser ("light" sandbox).
	// Implementations that have no browser concept may ignore it.
	Browser *bool
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd oasis && go test ./sandbox/ -run TestCreateOpts_BrowserField 2>&1 | tail -10`
Expected: PASS.

- [ ] **Step 5: Stage**

```bash
git add oasis/sandbox/manager.go oasis/sandbox/manager_test.go
```

---

### Task A2: `WithoutBrowser()` option omits the 7 browser tools

The 7 browser tools in `Tools()` (`oasis/sandbox/tools.go:209-244`) are: `browserTool`, `screenshotTool`, `snapshotTool`, `pageTextTool`, `exportPDFTool`, `browserEvalTool`, `browserFindTool`. (`webSearchTool`/`httpFetchTool` are NOT browser tools — they stay.)

**Files:**
- Modify: `oasis/sandbox/tools.go` (`toolsConfig` lines 44–48; `Tools` 209–244; options 56–71)
- Test: `oasis/sandbox/tools_test.go`

- [ ] **Step 1: Write the failing test**

Add to `oasis/sandbox/tools_test.go` (reuse the file's existing `mockSandbox`):

```go
func TestTools_WithoutBrowserOmitsBrowserTools(t *testing.T) {
	sb := &mockSandbox{}
	browserNames := map[string]bool{
		"browser": true, "screenshot": true, "snapshot": true,
		"page_text": true, "export_pdf": true, "browser_eval": true,
		"browser_find": true,
	}

	full := Tools(sb)
	var fullHasBrowser bool
	for _, tl := range full {
		if browserNames[tl.Definition().Name] {
			fullHasBrowser = true
		}
	}
	if !fullHasBrowser {
		t.Fatal("baseline Tools() should include browser tools")
	}

	light := Tools(sb, WithoutBrowser())
	for _, tl := range light {
		if browserNames[tl.Definition().Name] {
			t.Errorf("WithoutBrowser() leaked browser tool %q", tl.Definition().Name)
		}
	}
	// Non-browser tools must still be present.
	var hasShell, hasWebSearch bool
	for _, tl := range light {
		switch tl.Definition().Name {
		case "shell":
			hasShell = true
		case "web_search":
			hasWebSearch = true
		}
	}
	if !hasShell || !hasWebSearch {
		t.Errorf("WithoutBrowser() dropped non-browser tools: shell=%v web_search=%v", hasShell, hasWebSearch)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd oasis && go test ./sandbox/ -run TestTools_WithoutBrowserOmitsBrowserTools 2>&1 | tail -15`
Expected: FAIL — `undefined: WithoutBrowser`.

- [ ] **Step 3: Implement the option and gate the tools**

In `oasis/sandbox/tools.go`, add `noBrowser` to `toolsConfig`:

```go
type toolsConfig struct {
	delivery FileDelivery
	mounts   []MountSpec
	manifest *Manifest
	noBrowser bool
}
```

Add the option (next to `WithMounts`):

```go
// WithoutBrowser omits the browser tool set (browser, screenshot, snapshot,
// page_text, export_pdf, browser_eval, browser_find) from the returned tools.
// Use for "light" sandboxes that have no browser capability, so the model is
// never offered tools that would fail.
func WithoutBrowser() ToolsOption {
	return func(c *toolsConfig) { c.noBrowser = true }
}
```

Replace the tool-slice construction in `Tools()` so browser tools are conditional. Replace lines 215–235 (`tools := []oasis.AnyTool{ ... }`) with:

```go
	tools := []oasis.AnyTool{
		shellTool(sb),
		executeCodeTool(sb),
		fileReadTool(sb),
		fileWriteTool(sb, cfg),
		fileEditTool(sb, cfg),
		fileGlobTool(sb),
		fileGrepTool(sb),
		fileTreeTool(sb),
		httpFetchTool(sb),
		workspaceInfoTool(sb),
		mcpCallTool(sb),
		webSearchTool(sb),
	}

	if !cfg.noBrowser {
		tools = append(tools,
			browserTool(sb),
			screenshotTool(sb),
			snapshotTool(sb),
			pageTextTool(sb),
			exportPDFTool(sb),
			browserEvalTool(sb),
			browserFindTool(sb),
		)
	}
```

(The trailing `deliver_file` block at lines 237–242 stays unchanged after this.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd oasis && go test ./sandbox/ 2>&1 | tail -15`
Expected: PASS — the new test plus all existing sandbox tests (the reordering keeps every tool, just makes 7 conditional).

- [ ] **Step 5: Stage**

```bash
git add oasis/sandbox/tools.go oasis/sandbox/tools_test.go
```

---

# Phase B — daemon: `IX_BROWSER_MODE=disabled` → NoopBrowserBackend

Why: a "light" chat runs the **base** rootfs, which has **no pinchtab**. If such a daemon used `BrowserMode::Local` it would try to spawn pinchtab (absent) on boot. A `disabled` mode gives a deterministic, fast "browser unavailable" backend. Produces: `cargo test --all` green; `IX_BROWSER_MODE=disabled` yields `available()=false` and `Unavailable` on every browser route.

### Task B1: Parse `BrowserMode::Disabled`

**Files:**
- Modify: `daemon/crates/ix-core/src/config.rs` (`BrowserMode` enum + `from_env` parse + `ALL_KEYS`)
- Test: `daemon/crates/ix-core/src/config.rs` (inline `mod tests`)

- [ ] **Step 1: Write the failing test**

Add to the `mod tests` block in `config.rs`:

```rust
#[test]
fn browser_mode_disabled_parsed() {
    let cfg = with_env(&[("IX_BROWSER_MODE", "disabled")], DaemonConfig::from_env);
    assert_eq!(cfg.browser_mode, BrowserMode::Disabled);
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd daemon && cargo test -p ix-core browser_mode_disabled 2>&1 | tail -15`
Expected: FAIL — `no variant or associated item named Disabled`.

- [ ] **Step 3: Implement**

In `config.rs`, add the variant to `BrowserMode`:

```rust
pub enum BrowserMode {
    /// In-VM pinchtab (today's behaviour). Default.
    Local,
    /// Proxy browser calls to a shared Browser Gateway at this URL.
    Remote { gateway_url: String },
    /// No browser capability at all (light sandbox).
    Disabled,
}
```

In `from_env`, extend the match (the existing arm parses `remote=`):

```rust
        let browser_mode = match std::env::var("IX_BROWSER_MODE") {
            Ok(v) if v.starts_with("remote=") => BrowserMode::Remote {
                gateway_url: v["remote=".len()..].to_string(),
            },
            Ok(v) if v == "disabled" => BrowserMode::Disabled,
            _ => BrowserMode::Local,
        };
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd daemon && cargo test -p ix-core 2>&1 | tail -15`
Expected: PASS.

- [ ] **Step 5: Stage**

```bash
git add daemon/crates/ix-core/src/config.rs
```

---

### Task B2: `NoopBrowserBackend`

**Files:**
- Create: `daemon/crates/ix-browser/src/noop.rs`
- Modify: `daemon/crates/ix-browser/src/lib.rs`
- Test: `daemon/crates/ix-browser/src/noop.rs` (inline)

- [ ] **Step 1: Write the file with a failing test**

Create `daemon/crates/ix-browser/src/noop.rs`:

```rust
use async_trait::async_trait;
use ix_core::types::{
    BrowserAction, BrowserFindResult, BrowserResult, BrowserSnapshot, BrowserTextResult,
    NavigateResult, SnapshotOpts, TextOpts,
};
use ix_core::{Error, Result};

use crate::backend::BrowserBackend;

/// Browser backend for "light" sandboxes with no browser capability.
/// `available()` is false and every method returns `Error::Unavailable`,
/// so browser routes respond 503 without spawning Chrome/pinchtab.
pub struct NoopBrowserBackend;

impl NoopBrowserBackend {
    pub fn new() -> Self {
        NoopBrowserBackend
    }
}

impl Default for NoopBrowserBackend {
    fn default() -> Self {
        Self::new()
    }
}

fn unavailable<T>() -> Result<T> {
    Err(Error::Unavailable("browser disabled for this sandbox".into()))
}

#[async_trait]
impl BrowserBackend for NoopBrowserBackend {
    async fn navigate(&self, _url: &str) -> Result<NavigateResult> {
        unavailable()
    }
    async fn screenshot(&self) -> Result<Vec<u8>> {
        unavailable()
    }
    async fn action(&self, _action: BrowserAction) -> Result<BrowserResult> {
        unavailable()
    }
    async fn snapshot(&self, _opts: SnapshotOpts) -> Result<BrowserSnapshot> {
        unavailable()
    }
    async fn text(&self, _opts: TextOpts) -> Result<BrowserTextResult> {
        unavailable()
    }
    async fn pdf(&self) -> Result<Vec<u8>> {
        unavailable()
    }
    async fn eval(&self, _expr: &str) -> Result<String> {
        unavailable()
    }
    async fn find(&self, _query: &str) -> Result<BrowserFindResult> {
        unavailable()
    }
    fn available(&self) -> bool {
        false
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn noop_is_unavailable() {
        let b = NoopBrowserBackend::new();
        assert!(!b.available());
        assert!(matches!(
            b.navigate("https://example.com").await,
            Err(Error::Unavailable(_))
        ));
        assert!(matches!(b.screenshot().await, Err(Error::Unavailable(_))));
    }
}
```

- [ ] **Step 2: Export and run the failing test**

In `daemon/crates/ix-browser/src/lib.rs` add:

```rust
pub mod noop;
pub use noop::NoopBrowserBackend;
```

Run: `cd daemon && cargo test -p ix-browser noop_is_unavailable 2>&1 | tail -15`
Expected: PASS (the impl and test are written together; this confirms it compiles + behaves). If the `BrowserBackend` trait method set differs, fix `noop.rs` to match `backend.rs` exactly — do not change the trait.

- [ ] **Step 3: Stage**

```bash
git add daemon/crates/ix-browser/src/noop.rs daemon/crates/ix-browser/src/lib.rs
```

---

### Task B3: Wire `Disabled` selection in `ix-server`

**Files:**
- Modify: `daemon/crates/ix-server/src/main.rs` (the `match &config.browser_mode` block; currently has `Remote` + `Local` arms)

- [ ] **Step 1: Add the `Disabled` arm**

In `main.rs`, add to the existing `match &config.browser_mode { ... }` (alongside `Remote` and `Local`):

```rust
        ix_core::config::BrowserMode::Disabled => {
            info!("browser disabled for this sandbox (light mode)");
            pinchtab = None;
            Arc::new(ix_browser::NoopBrowserBackend::new())
        }
```

(`pinchtab = None;` matches the `Remote` arm so the later `if let Some(pinchtab) = pinchtab { pinchtab.shutdown().await; }` shutdown stays correct.)

- [ ] **Step 2: Build + full test**

Run: `cd daemon && cargo build -p ix-server 2>&1 | tail -15 && cargo test --all 2>&1 | tail -15`
Expected: compiles; all crates green.

- [ ] **Step 3: Stage**

```bash
git add daemon/crates/ix-server/src/main.rs
```

---

# Phase D — go-sdk: attach a second (state) drive

Why: Firecracker can't bind-mount a host dir; the browser-tier VM's pinchtab profile state must live on a second ext4 block device attached via `/drives`. `startVMCold` (`vmm.go:117`) currently attaches only `/drives/rootfs`. Produces: a unit-testable drive-spec builder + an `extraDrives` param threaded through `startVMCold`, with all existing callers passing `nil` (no behavior change).

### Task D1: `driveSpec` builder (pure, unit-tested)

**Files:**
- Modify: `go-sdk/vmm.go` (add `driveSpec` type + `buildDriveSpec` helper near the top, after `VMMHandle`)
- Test: `go-sdk/vmm_test.go`

- [ ] **Step 1: Write the failing test**

Add to `go-sdk/vmm_test.go`:

```go
func TestBuildDriveSpec(t *testing.T) {
	spec := buildDriveSpec("state", "/var/lib/ix/state.ext4", false)
	if spec["drive_id"] != "state" {
		t.Errorf("drive_id = %v, want state", spec["drive_id"])
	}
	if spec["path_on_host"] != "/var/lib/ix/state.ext4" {
		t.Errorf("path_on_host = %v", spec["path_on_host"])
	}
	if spec["is_root_device"] != false {
		t.Errorf("is_root_device = %v, want false", spec["is_root_device"])
	}
	if spec["is_read_only"] != false {
		t.Errorf("is_read_only = %v, want false", spec["is_read_only"])
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd go-sdk && go test -run TestBuildDriveSpec . 2>&1 | tail -10`
Expected: FAIL — `undefined: buildDriveSpec`.

- [ ] **Step 3: Implement**

In `go-sdk/vmm.go`, after the `VMMHandle` struct, add:

```go
// driveSpec is a Firecracker /drives/{id} PUT body.
type driveSpec map[string]any

// buildDriveSpec builds a non-root Firecracker drive body. readOnly toggles
// is_read_only; is_root_device is always false (rootfs uses its own setup).
func buildDriveSpec(id, pathOnHost string, readOnly bool) driveSpec {
	return driveSpec{
		"drive_id":       id,
		"path_on_host":   pathOnHost,
		"is_root_device": false,
		"is_read_only":   readOnly,
	}
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd go-sdk && go test -run TestBuildDriveSpec . 2>&1 | tail -10`
Expected: PASS.

- [ ] **Step 5: Stage**

```bash
git add go-sdk/vmm.go go-sdk/vmm_test.go
```

---

### Task D2: Thread `extraDrives` through `startVMCold` / `startVM`

**Files:**
- Modify: `go-sdk/vmm.go` (`startVM` signature 98–103; `startVMCold` signature 117 + the rootfs-drive block 174–184; add extra-drive PUTs after it)
- Modify: callers of `startVM`/`startVMCold` in `go-sdk/manager.go` and `go-sdk/snapshot.go` to pass `nil`

- [ ] **Step 1: Update signatures and add the extra-drive loop**

In `vmm.go`, change `startVM`:

```go
func (fb *firecrackerBackend) startVM(ctx context.Context, sandboxID string, vcpus int, memMB int64, rootfsImage string, envSlice []string, extraDrives []driveSpec) (*VMMHandle, error) {
	if fb.snapshot != nil && fb.snapshot.Ready() && rootfsImage == fb.rootfsImage {
		return fb.snapshot.Restore(ctx, sandboxID)
	}
	return fb.startVMCold(ctx, sandboxID, vcpus, memMB, rootfsImage, envSlice, extraDrives)
}
```

Change `startVMCold`'s signature to add `extraDrives []driveSpec`. Then, immediately AFTER the existing rootfs-drive PUT block (`fcPut(ctx, apiClient, "/drives/rootfs", ...)`, ending at line ~184) and BEFORE the `/machine-config` PUT, insert:

```go
	// Configure: extra (non-root) drives, e.g. the browser-tier state disk.
	for _, d := range extraDrives {
		id, _ := d["drive_id"].(string)
		if err := fcPut(ctx, apiClient, "/drives/"+id, d); err != nil {
			_ = cmd.Process.Kill()
			killPID(passtPID)
			_ = os.RemoveAll(socketDir)
			return nil, fmt.Errorf("set drive %s: %w", id, err)
		}
	}
```

- [ ] **Step 2: Update all callers to pass `nil`**

In `go-sdk/manager.go` (`Create`, ~line 263 area where `m.vmm.startVM(...)` is called): add a trailing `nil` argument:

```go
	handle, err := m.vmm.startVM(ctx, sandboxID, vcpus, memMB, resolved.Image, envSlice, nil)
```

Search for any other `startVM(` / `startVMCold(` callers (pool replenisher, snapshot golden-create) and add the trailing `nil`. Run the compiler to find them all (Step 3).

- [ ] **Step 3: Build to find remaining callers**

Run: `cd go-sdk && go build ./... 2>&1 | tail -20`
Expected: either clean, or `not enough arguments` errors pointing at the remaining callers. Fix each by appending `, nil`, then rebuild until clean.

- [ ] **Step 4: Full package test (no regressions)**

Run: `cd go-sdk && go test -count=1 . 2>&1 | tail -15`
Expected: PASS (existing behavior unchanged; extra-drive loop is a no-op for `nil`).

- [ ] **Step 5: Stage**

```bash
git add go-sdk/vmm.go go-sdk/manager.go go-sdk/snapshot.go
```

---

# Phase F — rootfs: `browser-vm` tier

Why: `build-rootfs-ext4.sh` (`VALID_TIERS=("base" "browser" "full")`) always installs `ixd` + `/sbin/ix-init`. The browser-tier image must instead use `browser-vm-init` as PID 1 and **not** rely on ixd. Produces: `IX_ROOTFS_IMAGE=ix-browser-vm.ext4 ./build-rootfs-ext4.sh browser-vm` builds a bootable browser-tier rootfs.

> This phase's output is verified by booting (Phase E integration). The script edits themselves are mechanical; review against the existing functions in the file.

### Task F1: Add the `browser-vm` tier branch

**Files:**
- Modify: `go-sdk/scripts/build-rootfs-ext4.sh`

- [ ] **Step 1: Allow the tier and map it to the Docker stage**

In `build-rootfs-ext4.sh`:

- Extend `VALID_TIERS` (line 10):
  ```bash
  readonly VALID_TIERS=("base" "browser" "full" "browser-vm")
  ```
- The image tag is `ix:${TIER}` (line 11) → build it as `ix:browser-vm` from the `browser-vm` Docker stage before running the script (documented in Step 4).

- [ ] **Step 2: Skip ixd + ix-init for the browser-vm tier**

The browser-vm image already ships `/usr/local/bin/browser-vm-init` (from the Dockerfile) and has no ixd. In `main()`, guard the ixd/init installation so it is skipped for `browser-vm`. Replace the `copy_daemon_binary` + `create_init_script` calls in `main()` with:

```go
  # Populate rootfs
  create_directories "$TEMP_ROOTFS"
  if [[ "$TIER" == "browser-vm" ]]; then
    echo "browser-vm tier: skipping ixd + ix-init (PID 1 is browser-vm-init from the image)"
    install_browser_vm_init "$TEMP_ROOTFS"
  else
    copy_daemon_binary "$TEMP_ROOTFS"
    create_init_script "$TEMP_ROOTFS"
  fi
```

(Note: shell, not Go — the ```go fence is for highlighting only; write plain bash.)

- [ ] **Step 3: Add `install_browser_vm_init` and point the kernel at it**

Add this function near `create_init_script`:

```bash
install_browser_vm_init() {
  local temp_dir="$1"
  # The browser-vm Docker stage already COPYs browser-vm-init to
  # /usr/local/bin/browser-vm-init and chmod +x it. Verify it is present so a
  # mis-tagged image fails loudly instead of producing an unbootable rootfs.
  if [[ ! -x "${temp_dir}/usr/local/bin/browser-vm-init" ]]; then
    echo "Error: browser-vm rootfs missing /usr/local/bin/browser-vm-init — did you build the browser-vm Docker stage?" >&2
    return 1
  fi
  # Symlink /sbin/ix-init -> browser-vm-init so a fixed kernel init= path works
  # for both tiers (kernel boot args use init=/sbin/ix-init).
  sudo ln -sf /usr/local/bin/browser-vm-init "${temp_dir}/sbin/ix-init"
  echo "✓ browser-vm init linked at /sbin/ix-init"
}
```

(Rationale: `buildKernelBootArgs` hardcodes `init=/sbin/ix-init` — `vmm.go:76`. Symlinking keeps the boot path uniform so the manager needs no per-tier init arg.)

- [ ] **Step 4: Document the build (no automated test — verified in Phase E)**

Record the build recipe in the plan's "Build & verify" section below. Manual smoke:
```bash
cd sandbox/ix
docker build --target browser-vm -f daemon/cmd/Dockerfile -t ix:browser-vm daemon/
IX_ROOTFS_IMAGE=/opt/ix/rootfs/browser-vm.ext4 IX_ROOTFS_SIZE=4096 \
  ./go-sdk/scripts/build-rootfs-ext4.sh browser-vm
file /opt/ix/rootfs/browser-vm.ext4   # expect: Linux ... ext4 filesystem data
```

- [ ] **Step 5: Stage**

```bash
git add go-sdk/scripts/build-rootfs-ext4.sh
```

---

# Phase C — go-sdk: config knobs, small default, light opt-out

Depends on A (uses `CreateOpts.Browser`). Produces: `ManagerConfig` carries browser-tier settings; per-chat default memory is 256 MB; `buildEnvSlice` emits `IX_BROWSER_MODE=disabled` for light chats and `remote=<url>` for browser chats.

### Task C1: Browser-tier config fields + 256 MB default

**Files:**
- Modify: `go-sdk/manager.go` (`ManagerConfig` struct ~28–46; `applyDefaults` ~49–79)
- Test: `go-sdk/manager_test.go`

- [ ] **Step 1: Write the failing test**

Add to `go-sdk/manager_test.go`:

```go
func TestApplyDefaults_PerChatMemory256(t *testing.T) {
	cfg := ManagerConfig{}
	cfg.applyDefaults()
	if want := int64(256 << 20); cfg.PerSandbox.Memory != want {
		t.Errorf("default per-sandbox memory = %d, want %d (256 MB)", cfg.PerSandbox.Memory, want)
	}
}

func TestApplyDefaults_BrowserTierMemoryDefault(t *testing.T) {
	cfg := ManagerConfig{BrowserMode: "remote"}
	cfg.applyDefaults()
	if cfg.BrowserVMMemoryMB != 4096 {
		t.Errorf("default browser-tier memory = %d MB, want 4096", cfg.BrowserVMMemoryMB)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd go-sdk && go test -run 'TestApplyDefaults_PerChatMemory256|TestApplyDefaults_BrowserTierMemoryDefault' . 2>&1 | tail -15`
Expected: FAIL — `unknown field BrowserMode/BrowserVMMemoryMB`, and the 256 assertion fails (current default is 512).

- [ ] **Step 3: Add fields + defaults**

In `ManagerConfig` (after the existing `BrowserGatewayURL string` field):

```go
	// Browser-tier (shared browser VM) settings. Active when BrowserMode=="remote".
	BrowserMode       string // "" / "local" (no tier) or "remote" (boot a shared browser-tier VM)
	BrowserVMImage    string // rootfs path for the browser-tier VM (browser-vm tier)
	BrowserVMMemoryMB int64  // browser-tier VM memory; default 4096
	BrowserStateImage string // optional ext4 state disk attached to the browser-tier VM; empty = ephemeral
	GatewayListenAddr string // host addr the gateway binds, reachable from guests via passt; default "169.254.0.1:9100"
	GatewayToken      string // optional bearer token forwarded to pinchtab and required from daemons
```

In `applyDefaults`, change the per-sandbox memory default from `512 << 20` to `256 << 20`:

```go
	if c.PerSandbox.Memory == 0 {
		c.PerSandbox.Memory = 256 << 20 // 256 MB (base image; was 512 for the heavy image)
	}
```

And append browser-tier defaults at the end of `applyDefaults`:

```go
	if c.BrowserMode == "remote" {
		if c.BrowserVMMemoryMB == 0 {
			c.BrowserVMMemoryMB = 4096
		}
		if c.GatewayListenAddr == "" {
			c.GatewayListenAddr = "169.254.0.1:9100"
		}
	}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd go-sdk && go test -run 'TestApplyDefaults' . 2>&1 | tail -15`
Expected: PASS.

> Note: lowering the default to 256 MB may affect `autoDetectMax` / pool tests that assume 512. Run the full package (`go test -count=1 .`) and adjust any test that hard-codes 512 MB to the new 256 MB default (these are test-data updates, not logic changes).

- [ ] **Step 5: Stage**

```bash
git add go-sdk/manager.go go-sdk/manager_test.go
```

---

### Task C2: `buildEnvSlice` honors the per-chat light/browser signal

Today `buildEnvSlice(userEnv, chatID)` sets `IX_BROWSER_MODE=remote=...` whenever `cfg.BrowserGatewayURL != ""`. A light chat must instead get `IX_BROWSER_MODE=disabled`. We add a `browser *bool` param mirroring `CreateOpts.Browser`.

**Files:**
- Modify: `go-sdk/manager.go` (`buildEnvSlice` 426–451; its callers: `Create` ~263, pool replenisher ~638, `health.go:95`)
- Test: `go-sdk/gateway_wiring_test.go` (extend; reuses `buildMinimalManager`)

- [ ] **Step 1: Write the failing tests**

Add to `go-sdk/gateway_wiring_test.go`:

```go
func TestBuildEnvSlice_LightChatDisablesBrowser(t *testing.T) {
	m := buildMinimalManager(ManagerConfig{
		BrowserGatewayURL: "http://gw:9100",
	})
	no := false
	env := m.buildEnvSlice(nil, "chat-light", &no)

	if !sliceContains(env, "IX_BROWSER_MODE=disabled") {
		t.Errorf("light chat should get IX_BROWSER_MODE=disabled; got %v", env)
	}
	for _, e := range env {
		if strings.HasPrefix(e, "IX_BROWSER_MODE=remote=") {
			t.Errorf("light chat must not get remote mode; got %v", env)
		}
	}
}

func TestBuildEnvSlice_BrowserChatUsesRemote(t *testing.T) {
	const gw = "http://gw:9100"
	m := buildMinimalManager(ManagerConfig{BrowserGatewayURL: gw})
	yes := true
	env := m.buildEnvSlice(nil, "chat-b", &yes)
	if !sliceContains(env, "IX_BROWSER_MODE=remote="+gw) {
		t.Errorf("browser chat should get remote mode; got %v", env)
	}
}

func TestBuildEnvSlice_NilBrowserDefaultsToRemoteWhenGatewaySet(t *testing.T) {
	const gw = "http://gw:9100"
	m := buildMinimalManager(ManagerConfig{BrowserGatewayURL: gw})
	env := m.buildEnvSlice(nil, "chat-d", nil) // nil = manager default
	if !sliceContains(env, "IX_BROWSER_MODE=remote="+gw) {
		t.Errorf("nil Browser with gateway set should default to remote; got %v", env)
	}
}
```

Also update the three EXISTING calls in this file (`m.buildEnvSlice(nil, chatID)` etc.) to pass a trailing `nil` so the file compiles: `m.buildEnvSlice(nil, chatID, nil)`, `m.buildEnvSlice(nil, "some-id", nil)`, `m.buildEnvSlice(nil, "", nil)`, `m.buildEnvSlice(map[string]string{"MY_VAR": "hello"}, "chat1", nil)`.

- [ ] **Step 2: Run to verify it fails**

Run: `cd go-sdk && go test -run TestBuildEnvSlice . 2>&1 | tail -20`
Expected: FAIL — `too many arguments in call to m.buildEnvSlice`.

- [ ] **Step 3: Implement the new signature + logic**

Replace `buildEnvSlice` (426–451) with:

```go
// buildEnvSlice constructs the env var slice to pass to ix-vmm.
// chatID is the per-chat routing key (omitted for pool entries with no chat).
// browser mirrors CreateOpts.Browser: nil = manager default, true = browser via
// the shared tier, false = light (browser disabled).
func (m *IXManager) buildEnvSlice(userEnv map[string]string, chatID string, browser *bool) []string {
	var envSlice []string
	for k, v := range userEnv {
		envSlice = append(envSlice, k+"="+v)
	}

	if m.cfg.DefaultEgress != nil && m.cfg.DefaultEgress.Enabled {
		envSlice = append(envSlice, "IX_EGRESS_ENABLED=true")
		envSlice = append(envSlice, "IX_EGRESS_MODE="+m.cfg.DefaultEgress.Mode)
		if len(m.cfg.DefaultEgress.Rules) > 0 {
			envSlice = append(envSlice, "IX_EGRESS_RULES="+strings.Join(m.cfg.DefaultEgress.Rules, ","))
		}
	}

	switch {
	case browser != nil && !*browser:
		// Light sandbox: explicitly disable browser so the base-image daemon
		// does not attempt in-VM pinchtab.
		envSlice = append(envSlice, "IX_BROWSER_MODE=disabled")
	case m.cfg.BrowserGatewayURL != "":
		// Browser via shared tier (nil = manager default, or explicit true).
		envSlice = append(envSlice, "IX_BROWSER_MODE=remote="+m.cfg.BrowserGatewayURL)
		if chatID != "" {
			envSlice = append(envSlice, "IX_CHAT_ID="+chatID)
		}
		if m.cfg.GatewayToken != "" {
			envSlice = append(envSlice, "IX_BROWSER_GATEWAY_TOKEN="+m.cfg.GatewayToken)
		}
	}

	return envSlice
}
```

- [ ] **Step 4: Update production callers**

- `Create` (~line 263): `envSlice := m.buildEnvSlice(resolved.Env, resolved.SessionID, resolved.Browser)`
- pool replenisher (~line 638): `envSlice := m.buildEnvSlice(nil, "", nil)` (pool entries are uncommitted; nil → default).
- `health.go:95`: `envSlice := m.buildEnvSlice(nil, sessionID, nil)` (restart reuses default; if you later persist per-chat Browser, thread it here too).

- [ ] **Step 5: Run to verify pass + no regressions**

Run: `cd go-sdk && go test -count=1 . 2>&1 | tail -15`
Expected: PASS — new tests green, all existing green.

- [ ] **Step 6: Stage**

```bash
git add go-sdk/manager.go go-sdk/gateway_wiring_test.go go-sdk/health.go
```

---

# Phase E — go-sdk: boot the browser-tier VM + serve the Gateway

Depends on C (config), D (state drive), and the existing gateway library. Produces: when `BrowserMode=="remote"`, `NewManager` boots one browser-tier VM, serves `Gateway` on `GatewayListenAddr`, and sets `cfg.BrowserGatewayURL` so per-chat VMs reach it; `Close` tears it all down.

> The boot path touches Firecracker and is exercised end-to-end by the integration test (Task E3, build-tagged). E1 is a pure unit (gateway URL derivation); E2 is the lifecycle wiring verified by compile + the integration test.

### Task E1: Derive the gateway URL from the listen addr (pure)

**Files:**
- Create: `go-sdk/browser_tier.go`
- Test: `go-sdk/browser_tier_test.go`

- [ ] **Step 1: Write the failing test**

Create `go-sdk/browser_tier_test.go`:

```go
package ix

import "testing"

func TestGatewayURLFromAddr(t *testing.T) {
	cases := map[string]string{
		"169.254.0.1:9100": "http://169.254.0.1:9100",
		"0.0.0.0:9100":     "http://0.0.0.0:9100",
		"":                 "http://169.254.0.1:9100",
	}
	for in, want := range cases {
		if got := gatewayURLFromAddr(in); got != want {
			t.Errorf("gatewayURLFromAddr(%q) = %q, want %q", in, got, want)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd go-sdk && go test -run TestGatewayURLFromAddr . 2>&1 | tail -10`
Expected: FAIL — `undefined: gatewayURLFromAddr`.

- [ ] **Step 3: Implement (file skeleton + helper)**

Create `go-sdk/browser_tier.go`:

```go
package ix

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// browserTier owns the shared browser-tier VM and its Gateway HTTP server.
type browserTier struct {
	vmm      *VMMHandle
	gateway  *Gateway
	server   *http.Server
	listener net.Listener
}

// gatewayURLFromAddr turns a bind address into the URL per-chat daemons use.
// Empty addr falls back to the default bind.
func gatewayURLFromAddr(addr string) string {
	if addr == "" {
		addr = "169.254.0.1:9100"
	}
	return "http://" + addr
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd go-sdk && go test -run TestGatewayURLFromAddr . 2>&1 | tail -10`
Expected: PASS.

- [ ] **Step 5: Stage**

```bash
git add go-sdk/browser_tier.go go-sdk/browser_tier_test.go
```

---

### Task E2: Boot + serve + teardown wiring

**Files:**
- Modify: `go-sdk/browser_tier.go` (add `startBrowserTier` + `stop`)
- Modify: `go-sdk/manager.go` (`IXManager` struct: add `tier *browserTier`; `NewManager` boot; `Close` teardown)

- [ ] **Step 1: Implement `startBrowserTier` and `stop`**

Append to `go-sdk/browser_tier.go`:

```go
// startBrowserTier boots the browser-tier VM, waits for pinchtab to become
// healthy over vsock (no READY handshake — pinchtab doesn't send one), then
// serves the Gateway on cfg.GatewayListenAddr. On success it returns a
// browserTier and the gateway URL per-chat daemons should use.
func startBrowserTier(ctx context.Context, fb *firecrackerBackend, cfg ManagerConfig) (*browserTier, string, error) {
	if cfg.BrowserVMImage == "" {
		return nil, "", fmt.Errorf("BrowserMode=remote requires BrowserVMImage")
	}

	// Optional persistent state disk as a second drive.
	var extra []driveSpec
	if cfg.BrowserStateImage != "" {
		extra = append(extra, buildDriveSpec("state", cfg.BrowserStateImage, false))
	}

	env := []string{}
	if cfg.GatewayToken != "" {
		env = append(env, "PINCHTAB_TOKEN="+cfg.GatewayToken)
	}

	handle, err := fb.startVMCold(ctx, "browser-tier", 2, cfg.BrowserVMMemoryMB, cfg.BrowserVMImage, env, extra)
	if err != nil {
		return nil, "", fmt.Errorf("boot browser-tier VM: %w", err)
	}

	// Wait for pinchtab /health via the vsock transport (mirrors snapshot.Restore).
	guestHTTP := &http.Client{Transport: vsockTransport(handle.VsockPath), Timeout: 2 * time.Second}
	if err := waitHealthy(ctx, guestHTTP); err != nil {
		fb.cleanup(handle)
		return nil, "", fmt.Errorf("browser-tier pinchtab health: %w", err)
	}

	gw := NewGatewayForBrowserVM(handle.VsockPath, cfg.GatewayToken, cfg.MaxInflightOrDefault(), cfg.Logger)
	gw.Start(ctx)

	ln, err := net.Listen("tcp", cfg.GatewayListenAddr)
	if err != nil {
		gw.Stop()
		fb.cleanup(handle)
		return nil, "", fmt.Errorf("gateway listen %s: %w", cfg.GatewayListenAddr, err)
	}
	srv := &http.Server{Handler: gw.Handler()}
	go func() { _ = srv.Serve(ln) }()

	cfg.Logger.Info("browser tier up", "vm_cid", handle.CID, "gateway", cfg.GatewayListenAddr)
	return &browserTier{vmm: handle, gateway: gw, server: srv, listener: ln}, gatewayURLFromAddr(cfg.GatewayListenAddr), nil
}

func (t *browserTier) stop(fb *firecrackerBackend) {
	if t == nil {
		return
	}
	if t.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = t.server.Shutdown(ctx)
		cancel()
	}
	if t.gateway != nil {
		t.gateway.Stop()
	}
	if t.vmm != nil {
		fb.cleanup(t.vmm)
	}
}
```

Add a small helper to `manager.go` (near `applyDefaults`) so the gateway in-flight cap is centralized:

```go
// MaxInflightOrDefault returns the per-VM browser in-flight cap (heuristic from
// the browser-tier memory: ~1 Chrome per 250 MB), min 1.
func (c ManagerConfig) MaxInflightOrDefault() int {
	n := int(c.BrowserVMMemoryMB / 250)
	if n < 1 {
		n = 1
	}
	return n
}
```

- [ ] **Step 2: Wire into `NewManager`**

In `manager.go`, add a field to `IXManager`:

```go
	tier *browserTier
```

In `NewManager`, AFTER `m.accepting.Store(true)` and BEFORE the `go m.monitor(mCtx)` line, add:

```go
	if cfg.BrowserMode == "remote" {
		tier, gwURL, err := startBrowserTier(mCtx, m.vmm, cfg)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("start browser tier: %w", err)
		}
		m.tier = tier
		m.cfg.BrowserGatewayURL = gwURL // per-chat VMs now point at the gateway
	}
```

- [ ] **Step 3: Wire teardown into `Close`**

In `Close` (`manager.go:340`), after the pool-entry cleanup loop and before `return firstErr`, add:

```go
	m.tier.stop(m.vmm)
```

- [ ] **Step 4: Compile**

Run: `cd go-sdk && go build ./... 2>&1 | tail -20`
Expected: clean. (Confirms `cleanup`, `waitHealthy`, `NewGatewayForBrowserVM`, `startVMCold` signatures all line up.)

- [ ] **Step 5: Full unit suite (tier not booted without remote mode)**

Run: `cd go-sdk && go test -count=1 . 2>&1 | tail -15`
Expected: PASS — no existing test sets `BrowserMode=="remote"`, so the tier boot path is dormant in unit tests.

- [ ] **Step 6: Stage**

```bash
git add go-sdk/browser_tier.go go-sdk/manager.go
```

---

### Task E3: End-to-end integration test (build-tagged)

**Files:**
- Create: `go-sdk/browser_tier_integration_test.go` (build tag `//go:build integration`)

> Requires a host with Firecracker + KVM + the two rootfs images built (Phases F + base). Mirrors the existing serial integration-test convention.

- [ ] **Step 1: Write the integration test**

Create `go-sdk/browser_tier_integration_test.go`:

```go
//go:build integration

package ix_test

import (
	"context"
	"os"
	"testing"
	"time"

	ix "github.com/nevindra/ix/go-sdk"
	"github.com/nevindra/oasis/sandbox"
)

// Requires env: SANDBOX_ROOTFS (base ext4), SANDBOX_KERNEL, SANDBOX_BROWSER_ROOTFS (browser-vm ext4).
func TestBrowserTierEndToEnd(t *testing.T) {
	base := os.Getenv("SANDBOX_ROOTFS")
	kernel := os.Getenv("SANDBOX_KERNEL")
	browserImg := os.Getenv("SANDBOX_BROWSER_ROOTFS")
	if base == "" || kernel == "" || browserImg == "" {
		t.Skip("set SANDBOX_ROOTFS, SANDBOX_KERNEL, SANDBOX_BROWSER_ROOTFS to run")
	}

	ctx := context.Background()
	mgr, err := ix.NewManager(ctx, ix.ManagerConfig{
		RootfsImage:       base,
		KernelPath:        kernel,
		BrowserMode:       "remote",
		BrowserVMImage:    browserImg,
		BrowserVMMemoryMB: 4096,
		GatewayListenAddr: "127.0.0.1:9100",
	})
	if err != nil {
		t.Fatalf("NewManager (with browser tier): %v", err)
	}
	defer mgr.Close()

	// Browser-enabled small chat: navigate must succeed via the shared tier.
	yes := true
	sb, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "it-browser", TTL: 5 * time.Minute, Browser: &yes})
	if err != nil {
		t.Fatalf("create browser sandbox: %v", err)
	}
	if err := sb.BrowserNavigate(ctx, "https://example.com"); err != nil {
		t.Fatalf("navigate via shared tier: %v", err)
	}

	// Light chat: WorkspaceInfo reports browser unavailable.
	no := false
	light, err := mgr.Create(ctx, sandbox.CreateOpts{SessionID: "it-light", TTL: 5 * time.Minute, Browser: &no})
	if err != nil {
		t.Fatalf("create light sandbox: %v", err)
	}
	info, err := light.WorkspaceInfo(ctx)
	if err != nil {
		t.Fatalf("workspace info: %v", err)
	}
	if info.Browser {
		t.Error("light sandbox should report Browser=false")
	}
}
```

- [ ] **Step 2: Run (on a Firecracker host)**

Run:
```bash
cd go-sdk
docker build --target base       -f ../daemon/cmd/Dockerfile -t ix:base       ../daemon/
docker build --target browser-vm -f ../daemon/cmd/Dockerfile -t ix:browser-vm ../daemon/
IX_ROOTFS_IMAGE=/opt/ix/rootfs/base.ext4       ./scripts/build-rootfs-ext4.sh base
IX_ROOTFS_IMAGE=/opt/ix/rootfs/browser-vm.ext4 IX_ROOTFS_SIZE=4096 ./scripts/build-rootfs-ext4.sh browser-vm
SANDBOX_ROOTFS=/opt/ix/rootfs/base.ext4 \
SANDBOX_KERNEL=/opt/ix/vmlinux \
SANDBOX_BROWSER_ROOTFS=/opt/ix/rootfs/browser-vm.ext4 \
  go test -tags=integration -run TestBrowserTierEndToEnd -count=1 . 2>&1 | tail -25
```
Expected: PASS — navigate succeeds via the tier; light sandbox reports `Browser=false`.

- [ ] **Step 3: Stage**

```bash
git add go-sdk/browser_tier_integration_test.go
```

---

# Phase G — athena: wiring

Depends on A (`CreateOpts.Browser`, `WithoutBrowser`) and C (`ManagerConfig` browser-tier fields). Produces: athena boots with a small base image by default, a shared browser tier when configured, and per-agent browser opt-out.

> athena's go.mod pins `github.com/nevindra/ix/go-sdk v0.1.1` and `github.com/nevindra/oasis v0.17.3`. To consume the unreleased oasis/go-sdk changes, add local `replace` directives during development (Task G0).

### Task G0: Local module replaces (dev only)

**Files:**
- Modify: `athena-new/go.mod`

- [ ] **Step 1: Add replace directives**

Append to `athena-new/go.mod`:

```
replace github.com/nevindra/oasis => /home/nezhifi/Development/oasis
replace github.com/nevindra/ix/go-sdk => /home/nezhifi/Development/sandbox/ix/go-sdk
```

- [ ] **Step 2: Verify resolution**

Run: `cd athena-new && go build ./... 2>&1 | tail -20`
Expected: builds against the local modules (no version errors).

- [ ] **Step 3: Stage**

```bash
git add athena-new/go.mod athena-new/go.sum
```

---

### Task G1: Map `SANDBOX_BROWSER_*` env → `ManagerConfig`

**Files:**
- Modify: `athena-new/main.go` (the `ix.ManagerConfig{...}` literal, lines 172–183)

- [ ] **Step 1: Extend the config literal**

In `main.go`, add to the `ixCfg := ix.ManagerConfig{ ... }` literal:

```go
			BrowserMode:       os.Getenv("SANDBOX_BROWSER_MODE"),       // "remote" enables the shared tier
			BrowserVMImage:    os.Getenv("SANDBOX_BROWSER_ROOTFS"),
			BrowserVMMemoryMB: int64(envInt("SANDBOX_BROWSER_MEMORY_MB", 0)),
			BrowserStateImage: os.Getenv("SANDBOX_BROWSER_STATE_IMAGE"),
			GatewayListenAddr: os.Getenv("SANDBOX_GATEWAY_ADDR"),
			GatewayToken:      os.Getenv("SANDBOX_GATEWAY_TOKEN"),
```

(`SANDBOX_ROOTFS` continues to point at the small base image. `applyDefaults` fills `BrowserVMMemoryMB`/`GatewayListenAddr` when `BrowserMode=="remote"`.)

- [ ] **Step 2: Build**

Run: `cd athena-new && go build ./... 2>&1 | tail -15`
Expected: clean.

- [ ] **Step 3: Stage**

```bash
git add athena-new/main.go
```

---

### Task G2: Per-agent `NeedsBrowser` flag

**Files:**
- Modify: `athena-new/internal/features/agents/model.go` (`Agent` struct 8–24)

- [ ] **Step 1: Add the field**

In `model.go`, add to `Agent`:

```go
	NeedsBrowser  *bool           `json:"needs_browser,omitempty"`
```

(`*bool`: nil = default/browser-via-tier, false = light, true = browser. No DB migration needed if `adapter_config` already carries free-form config; if `Agent` maps to a column, add a nullable `needs_browser BOOLEAN` migration in athena's `db/` following the existing dbmate convention.)

- [ ] **Step 2: Build**

Run: `cd athena-new && go build ./... 2>&1 | tail -15`
Expected: clean.

- [ ] **Step 3: Stage**

```bash
git add athena-new/internal/features/agents/model.go
```

---

### Task G3: Thread `NeedsBrowser` → `CreateOpts.Browser` + `WithoutBrowser()`

**Files:**
- Modify: `athena-new/internal/adapter/adapter.go` (`Request` struct 20–35) — add `NeedsBrowser *bool`
- Modify: `athena-new/internal/adapter/oasis.go` (the two `sandbox.CreateOpts{...}` sites: `executeSingleAgent` ~390 and `newLazySandbox` in `executeTree` ~490; the two `sandbox.Tools(...)` sites ~419 and ~631)
- Test: `athena-new/internal/adapter/oasis_test.go`

- [ ] **Step 1: Write the failing test**

Add to `athena-new/internal/adapter/oasis_test.go` a pure helper test. First introduce a tiny exported helper in `oasis.go` (so it's unit-testable without a manager):

```go
// browserToolOpts returns the sandbox.Tools options implied by a request's
// browser preference: WithoutBrowser() when NeedsBrowser is explicitly false.
func browserToolOpts(req Request) []sandbox.ToolsOption {
	if req.NeedsBrowser != nil && !*req.NeedsBrowser {
		return []sandbox.ToolsOption{sandbox.WithoutBrowser()}
	}
	return nil
}
```

Test:

```go
func TestBrowserToolOpts(t *testing.T) {
	no := false
	yes := true
	if opts := adapter.BrowserToolOpts(adapter.Request{NeedsBrowser: &no}); len(opts) != 1 {
		t.Errorf("NeedsBrowser=false should yield 1 opt (WithoutBrowser), got %d", len(opts))
	}
	if opts := adapter.BrowserToolOpts(adapter.Request{NeedsBrowser: &yes}); len(opts) != 0 {
		t.Errorf("NeedsBrowser=true should yield 0 browser opts, got %d", len(opts))
	}
	if opts := adapter.BrowserToolOpts(adapter.Request{}); len(opts) != 0 {
		t.Errorf("nil NeedsBrowser should yield 0 browser opts, got %d", len(opts))
	}
}
```

(Export the helper as `BrowserToolOpts` if the test is in `package adapter_test`; or keep it unexported and put the test in `package adapter`. Match the existing `oasis_test.go` package — it is `package adapter_test`, so name the helper `BrowserToolOpts`.)

- [ ] **Step 2: Run to verify it fails**

Run: `cd athena-new && go test ./internal/adapter/ -run TestBrowserToolOpts 2>&1 | tail -10`
Expected: FAIL — `undefined: adapter.BrowserToolOpts`.

- [ ] **Step 3: Implement**

- In `adapter.go`, add to `Request`:
  ```go
	NeedsBrowser *bool // nil = default (browser via shared tier); false = light; true = browser
  ```
- In `oasis.go`, add the exported helper:
  ```go
	// BrowserToolOpts returns sandbox.Tools options implied by the request's
	// browser preference: WithoutBrowser() when NeedsBrowser is explicitly false.
	func BrowserToolOpts(req Request) []sandbox.ToolsOption {
		if req.NeedsBrowser != nil && !*req.NeedsBrowser {
			return []sandbox.ToolsOption{sandbox.WithoutBrowser()}
		}
		return nil
	}
  ```
- At the `executeSingleAgent` create site (~390), set `Browser`:
  ```go
	sb, err = a.sandboxMgr.Create(ctx, sandbox.CreateOpts{
		SessionID: req.RunID,
		TTL:       time.Hour,
		Browser:   req.NeedsBrowser,
	})
  ```
- At the `executeTree` `newLazySandbox` site (~490):
  ```go
	sharedSandbox = newLazySandbox(a.sandboxMgr, sandbox.CreateOpts{
		SessionID: req.RunID,
		TTL:       time.Hour,
		Browser:   req.NeedsBrowser,
	}, mountSpecs, manifest)
  ```
- At BOTH `sandbox.Tools(...)` sites (~419 and ~631), prepend the browser opts. Replace `toolOpts := a.sandboxToolOpts()` with:
  ```go
	toolOpts := append(a.sandboxToolOpts(), BrowserToolOpts(req)...)
  ```

- [ ] **Step 4: Populate `Request.NeedsBrowser` from the agent**

Wherever athena builds the `adapter.Request` from an `Agent` (the run worker / chat executor that sets `AgentConfig`), set `NeedsBrowser: agent.NeedsBrowser`. Find the construction sites:

Run: `cd athena-new && go doc ./internal/adapter Request >/dev/null 2>&1; grep -rln "adapter.Request{" internal/ 2>/dev/null` — for each, add `NeedsBrowser: <agent>.NeedsBrowser,` where an `Agent` is in scope. (If grep is blocked by tooling, build will not catch missing optional fields — so audit `internal/features/runs/` and `internal/features/chat/` construction sites manually.)

- [ ] **Step 5: Build + test**

Run: `cd athena-new && go build ./... 2>&1 | tail -15 && go test ./internal/adapter/ 2>&1 | tail -15`
Expected: clean build; `TestBrowserToolOpts` + existing adapter tests pass.

- [ ] **Step 6: Stage**

```bash
git add athena-new/internal/adapter/adapter.go athena-new/internal/adapter/oasis.go athena-new/internal/adapter/oasis_test.go
```

---

## Build & verify (operator runbook)

```bash
# 1. Build both rootfs images (on a Firecracker host)
cd sandbox/ix
docker build --target base       -f daemon/cmd/Dockerfile -t ix:base       daemon/
docker build --target browser-vm -f daemon/cmd/Dockerfile -t ix:browser-vm daemon/
IX_ROOTFS_IMAGE=/opt/ix/rootfs/base.ext4        ./go-sdk/scripts/build-rootfs-ext4.sh base
IX_ROOTFS_IMAGE=/opt/ix/rootfs/browser-vm.ext4 IX_ROOTFS_SIZE=4096 \
  ./go-sdk/scripts/build-rootfs-ext4.sh browser-vm

# 2. (optional) create a persistent browser-state ext4 disk
dd if=/dev/zero of=/opt/ix/browser-state.ext4 bs=1M count=2048 && mkfs.ext4 -F /opt/ix/browser-state.ext4

# 3. Run athena pointed at the small base image + shared browser tier
export SANDBOX_ROOTFS=/opt/ix/rootfs/base.ext4
export SANDBOX_KERNEL=/opt/ix/vmlinux
export SANDBOX_BROWSER_MODE=remote
export SANDBOX_BROWSER_ROOTFS=/opt/ix/rootfs/browser-vm.ext4
export SANDBOX_BROWSER_MEMORY_MB=4096
export SANDBOX_BROWSER_STATE_IMAGE=/opt/ix/browser-state.ext4   # optional
export SANDBOX_GATEWAY_ADDR=169.254.0.1:9100
cd athena-new && go run .
```

Memory check (target): per-chat VM ≈ 256 MB; browser tier ≈ 4 GB; at 30 chats the compute tier ≈ 7.5 GB vs ~21 GB before.

---

## Self-Review

**Spec coverage:**
- Spec A (small base rootfs default) → Phase F (browser-vm tier) + Phase C1 (256 MB default) + G1 (`SANDBOX_ROOTFS` = base). ✓
- Spec B1 (image override + memory) → C1; B2 (tier lifecycle) → E2; B3 (gateway) → **already built**, served in E2; B4 (per-chat env) → **already built**, light path added in C2; B5 (WorkspaceInfo browser flag) → covered by daemon backends (`Disabled`→`available()=false` in B; `Remote`→true) and asserted in E3. ✓
- Spec C1 (`CreateOpts.Browser`) → A1; C2 (`WithoutBrowser`) → A2. ✓
- Spec D1/D2 (athena config + per-agent flag) → G1/G2/G3. ✓
- State persistence (second drive) → D + E2 (`BrowserStateImage`). ✓
- Light path needing a daemon "disabled" mode (not in original spec, discovered during grounding: base image has no pinchtab) → Phase B. ✓ (Spec updated rationale lives in the design doc's Current State.)

**Placeholder scan:** No TBD/TODO. Every code step shows real code; the two bash steps (F) are flagged as bash despite ```go fences (used only for highlighting).

**Type consistency:** `CreateOpts.Browser *bool` (A1) is read by `buildEnvSlice(..., browser *bool)` (C2) via `resolved.Browser` (C2 Step 4) and passed in `Create` (C2). `WithoutBrowser()` returns `ToolsOption` (A2) consumed by `BrowserToolOpts` (G3). `buildDriveSpec`→`driveSpec` (D1) consumed by `startVMCold(..., extraDrives []driveSpec)` (D2) and `startBrowserTier` (E2). `NewGatewayForBrowserVM(vsockUDS, token, maxInflight, logger)` (existing) called with `cfg.MaxInflightOrDefault()` (E2). `BrowserMode string` ("remote") consistent across C1/E2/G1.

**Known coordination risk:** Phase A/C land in different repos than their consumers; G0 adds local `replace` directives so athena builds against them during dev. Releasing oasis + go-sdk versions and bumping athena's go.mod is the productionization step (out of plan scope).

**Verified module paths:** go-sdk = `github.com/nevindra/ix/go-sdk` (confirmed via `go test` output `ok github.com/nevindra/ix/go-sdk`); oasis = `github.com/nevindra/oasis` (sandbox pkg `github.com/nevindra/oasis/sandbox`); athena = `github.com/nezhifi/orchestrator`. athena's go.mod pins `github.com/nevindra/ix/go-sdk v0.1.1` + `github.com/nevindra/oasis v0.17.3` — G0 replaces both with local paths.
