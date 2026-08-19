# PRD — Sandbox capacity and resource limits

Status: draft · 2026-08-13
Related: [ADR 0001](../adr/0001-admission-on-memory-alone.md), [ADR 0002](../adr/0002-no-pause-or-resume.md), [CONTEXT.md](../../CONTEXT.md)

## Problem

ix cannot reach its target of 50 concurrent sandboxes on its own reference hardware, and
when it is pushed near the limit it has no defence against a single sandbox taking the
host down with it.

Three defects combine:

1. **The concurrency ceiling is set by core count, not by what sandboxes actually
   consume.** `autoDetectMax` takes `min(logical CPUs, host memory / per-sandbox
   memory)`. On the reference machine the CPU term always wins: 32 on an AMD part with
   SMT, 16 on an Intel Core Ultra part without it. Both refuse the 33rd (or 17th)
   sandbox while most of the live ones are idle.

2. **The ceiling cannot be overridden by the operator.** `MaxConcurrent` exists in
   `ManagerConfig` but Athena never sets it and exposes no environment variable for it,
   so a deployment on a larger machine has no way to use it.

3. **Nothing bounds what one sandbox may take.** No cgroup limits are applied anywhere —
   Firecracker is launched as a bare process. Guest memory is faulted in lazily, so 50
   sandboxes configured at 512 MB look harmless while idle and only reveal the
   over-commit when several bursts land together. The failure mode is a kernel OOM kill
   that chooses its victim by score, not by fault.

Defect 3 is currently masked by defect 1: the low ceiling keeps the host away from the
cliff. Fixing the ceiling without fixing the limits would walk straight off it.

There is also a latent fourth: `hostMemoryBytes()` returns **total** system memory, so
the memory term reserves nothing for the OS, Postgres, SeaweedFS, the Athena process, or
the browser tier. It has never mattered because the CPU term was always smaller. Once
the CPU term is removed it becomes the entire formula.

## Who this is for

The **on-premise operator** — someone installing Athena on a machine ix does not
control, and who will not read ix's source to find out why it refused a sandbox. They
own the host; they need ix to tell them what it needs and to fail in ways they can act on.

Secondary: **Athena**, which must be able to pass the operator's capacity choice through
to ix.

## Reference deployment

Capacity claims are meaningless without the machine attached to them. This is the shape
ix targets:

| | |
|---|---|
| CPU | 16 physical cores (edge device class — AMD Ryzen AI Max, Intel Core Ultra — or a rack server) |
| RAM | 64 GB |
| Target concurrency | 50 sandboxes |
| Per sandbox | 512 MB (256 MB OOMs the skill toolchain: pandas, matplotlib, typst, duckdb) |
| Browser tier | 4 GB |
| Host (OS + Postgres + SeaweedFS + Athena) | ~4 GB |
| **Used at target** | **~34 GB of 64 GB** |

The unit ix should publish is **not "50 sandboxes"** — that number is only true on this
machine. It is **~512 MB per concurrent sandbox, plus ~8 GB for the host and the browser
tier**. Operators can size their own box from that, and it stays true on a mini PC and
on a rack.

## Goals

- Reach 50 concurrent sandboxes on the reference deployment.
- Make the ceiling identical on AMD and Intel parts with the same memory.
- Make a sandbox that exceeds its memory allowance fail by itself, with a diagnosable
  error, instead of endangering its neighbours.
- Make a sandbox that monopolises CPU degrade its neighbours proportionally instead of
  starving them.
- Let the operator discover a mis-sized configuration before it becomes an outage.

## Non-goals

- Faster sandbox creation. Not measured as a problem; see *Out of scope*.
- Surviving a host restart, or reviving a conversation's sandbox after it is gone
  (ADR 0002).
- Fair-share scheduling beyond proportional CPU weight. If "some fast" ever beats "all
  slower", that is a queue for heavy operations, not an admission policy.

## Requirements

### R1 — Admit on memory, after reserving host memory

Replace `min(CPU, memory)` with a memory-only calculation that subtracts a host reserve
before dividing.

*Rationale:* ADR 0001. Memory is the resource a sandbox holds for its whole life; CPU is
consumed only in bursts.

*Acceptance:*
- On the reference deployment, the auto-detected ceiling is the same on a 16c/32t AMD
  part and a 16c/16t Intel part.
- The ceiling never assumes memory the host itself needs; the reserve accounts for the
  browser tier when it is enabled.
- Dropping the CPU term without R1's reserve must not be shippable — they land together.

### R2 — Per-sandbox memory limit

Each sandbox runs under a cgroup memory limit derived from its configured memory plus a
small allowance for Firecracker's own overhead.

*Rationale:* R1 raises the ceiling; this is the guard rail that makes the higher ceiling
safe. Without it, over-commit fails as a random kernel OOM kill.

*Acceptance:*
- A sandbox whose guest grows past its allowance is killed, and the failure is
  attributable to that sandbox in logs.
- No other sandbox, and no host process, is affected.

### R3 — Per-sandbox CPU weight

Each sandbox runs under a proportional cgroup CPU weight.

*Rationale:* With admission no longer limited by cores, several sandboxes bursting
together must share cores by policy rather than by luck. A runaway loop must not count
for as much as 49 working sandboxes.

*Acceptance:*
- With more busy sandboxes than cores, CPU time is distributed proportionally.
- A single CPU-saturating sandbox does not measurably delay an idle sandbox's request
  handling.

> **Investigated — do not use jailer for R2/R3.**
>
> Firecracker's `jailer` (already installed alongside the pinned v1.15.1 binary) does
> expose what R2 and R3 need: `--cgroup-version 2 --cgroup memory.max=<bytes> --cgroup
> cpu.weight=<n>`, plus `--parent-cgroup`, `--netns`, `--new-pid-ns` and `--resource-limit`.
> Note `--cgroup-version` defaults to **1**, so cgroup v2 must be requested explicitly.
>
> But jailer chroots the VMM into `<chroot-base-dir>/<exec-file>/<id>/root` and needs
> privilege to do so before dropping to `--uid`/`--gid`. Adopting it would mean relocating
> every path a sandbox touches — rootfs, kernel, scratch disk, API socket, vsock UDS —
> into the jail, and it would reintroduce a root requirement that ix's rootless
> preconfigured-network mode exists specifically to avoid.
>
> Writing cgroup v2 directly costs neither. Verified unprivileged on a systemd host: the
> `cpu memory pids` controllers are delegated down the whole user slice, and an ordinary
> user can create child cgroups there. One structural rule applies — a cgroup with
> processes directly in it cannot enable controllers for its children — so ix must place
> its own process in a sub-cgroup before creating one cgroup per sandbox and moving each
> Firecracker PID into it. That is the same pattern container runtimes use, and it needs
> no chroot, no privilege escalation, and no path migration.
>
> Jailer remains worth revisiting as its own decision if the parked golden-snapshot work
> resumes: that needs per-sandbox network namespaces, and `--netns` alongside chroot,
> seccomp and a private PID namespace would then be earning their keep together.

### R4 — Operator override for the ceiling

Athena exposes the admission ceiling as an environment variable, passed to
`ManagerConfig.MaxConcurrent`.

*Rationale:* After R1 the auto-detected value is defensible, so this becomes an
emergency brake rather than required configuration — but a deployment with unusual
memory pressure needs a way to say so.

*Acceptance:* setting the variable changes the effective ceiling; leaving it unset uses
R1's calculation.

### R5 — `doctor` reports capacity arithmetic, not just presence

Extend the existing health surface into an operator-facing diagnostic that reports what
ix will do with the machine it is on, not merely whether its dependencies exist.

*Rationale:* The host is not ours. A mis-sized configuration should be caught at install
time, not at 2am under load.

*Acceptance:* output states host memory, per-sandbox allowance, browser-tier and host
reserve, the resulting ceiling, and whether any explicit override contradicts it — in
terms an operator can act on without reading the source. Network prerequisites
(ip_forward, the nft table, the TAP pool) are checked alongside the existing KVM, kernel,
rootfs, and binary checks.

### R6 — Resource metrics

Expose per-sandbox and aggregate resource metrics.

*Rationale:* At 50 neighbours, "it got slow" is unactionable without knowing which
sandbox grew.

*Acceptance:* current sandbox count against the ceiling, per-sandbox memory, and
admission refusals are all observable without shell access to the host.

### R7 — Measure real consumption — **done, and it changes R1 and R2**

`TestSandboxRSSProfile` (integration-tagged) records host RSS per Firecracker process
across a sandbox's life, with every sandbox bursting at once.

First run — 16 logical CPUs / 31 GB host, 6 sandboxes at 512 MiB configured each, each
allocating 256 MiB concurrently, vsock-only, on ix's `base.ext4`. Median MiB per sandbox:

| phase | RSS | note |
|---|---|---|
| born | 81 | immediately after create returned |
| idle | 82 | 30 s after boot, no requests |
| warm | 85 | Python kernel booted |
| burst | 351 | all six allocated 256 MiB at once |
| **settled** | **351** | **60 s later, guests idle again** |

Three findings, in order of how much they matter:

1. **Guest memory is never returned.** Sixty seconds after the workload finished, the
   host had reclaimed exactly zero. A sandbox's cost is not a level, it is a **ratchet**:
   it climbs toward its configured ceiling as the session goes on and stays there. Over a
   30-minute conversation with several document-generation bursts, every sandbox tends
   toward its full allowance.
2. **Host RSS tracks guest allocation almost 1:1** (85 idle + 256 allocated ≈ 351). There
   is no hidden amplification, and no hidden savings.
3. **An idle sandbox costs ~85 MiB, not 512 MiB** — about a sixth of its allowance. The
   over-commit headroom is real and large.

Findings 1 and 3 pull in opposite directions, and finding 1 wins: **admission cannot be
sized against observed averages.** Sizing on the 85 MiB idle figure would admit roughly
six times what the host can survive once sessions mature. Admission must be sized against
a ceiling — which only means something if R2 enforces it. This retires the option of
shipping R1 alone.

*Caveats on these numbers.* Measured on ix's `base.ext4`, which carries neither pandas,
matplotlib, nor typst — Athena's heavy rootfs will ratchet faster and further. The burst
was a synthetic allocation, not a real document build. Networking was disabled (TAP setup
needs root); a TAP device costs the guest no memory, but this has not been confirmed on a
provisioned host. The host was 31 GB, not the 64 GB reference. **Re-run on Athena's rootfs
before R1's reserve default is fixed.**

*Follow-up worth its own investigation:* Firecracker's balloon device supports **free page
reporting**, which `madvise(MADV_DONTNEED)`s ranges the guest reports as free and so
reduces VM RSS — a direct attack on finding 1, and the only thing that would make
average-based admission safe. It is a **developer-preview** feature and needs guest-kernel
support (`page_reporting_order`), so it is a research item, not a plan item.

## Sequencing

R7 is done, and its result removes a choice rather than adding one: because guest memory
never comes back, **R1 and R2 must ship together.** R1 alone would remove the accidental
safety of today's low ceiling and leave nothing bounding the ratchet in its place.

Then R3, then R4, then R5 and R6. R7 should be re-run on Athena's rootfs before R1's
reserve default is settled — the numbers here come from a lighter image than production
uses.

## Out of scope, and why

Recorded because these are the obvious things to copy from comparable runtimes, and the
reasons they do not apply here are not obvious.

- **Live branching / forking a running sandbox, diff snapshots, snapshot chains,
  snapshot distribution.** All of these presume the contents of a sandbox's memory are
  valuable and expensive to rebuild. In this system they are not: Oasis commits an
  agent's files out to durable storage after each tool call, so a sandbox never holds the
  only copy of anything.
- **Pause / resume.** ADR 0002.
- **A vendored Firecracker fork.** The upstream-divergence cost is permanent; the payoff
  is a snapshot pause window nobody in this system would notice.
- **MCP server, framework recipes, Python/TypeScript SDKs.** These serve adoption by
  people outside this project. ix is not distributed on its own today. Nothing here
  forecloses adding them later.
- **Golden snapshot work (parked, not rejected).** It could still shorten sandbox
  creation, but restored sandboxes currently have no network at all, and fixing that
  needs per-sandbox network namespaces. Do not start until R7 shows creation latency is
  a real problem.

Already solved, and worth not re-solving: on-premise distribution. The bundle packaging
and the paired `IX_BUNDLE_VERSION` / `ATHENA_ROOTFS_VERSION` pins already cover getting
ix onto a machine.

## Risks

- **Sizing against the ceiling wastes most of the box in the common case.** R7 measured an
  idle sandbox at ~85 MiB against a 512 MiB allowance, so admission sized on the ceiling
  refuses sandboxes a host could comfortably hold — right up until those sandboxes mature
  and it could not. This is accepted deliberately: the alternative fails as an outage
  rather than a refusal. Free page reporting (see R7) is the only known way to recover the
  headroom safely.
- **All-burst slowdown.** Accepted deliberately (ADR 0001). It becomes a product problem
  only if bursts stop being rare — worth watching via R6.
- **Thermal ceilings on edge devices.** The reference AMD part defaults to a 55 W power
  budget (configurable 45–120 W) for 16 Zen 5 cores plus a large iGPU. Sustained all-core
  load throttles clocks well below boost. Capacity planning by core count and memory will
  overstate what a fanless or thin-chassis device delivers.
- **Heterogeneous cores.** Intel Core Ultra parts mix performance and efficiency cores,
  and ix pins no vCPU affinity. Two identical sandboxes on the same host can perform
  measurably differently. Not addressed here; `doctor` could eventually warn.
