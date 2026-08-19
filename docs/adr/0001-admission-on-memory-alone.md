# Sandbox admission is decided on memory alone

Status: accepted

ix caps concurrent sandboxes at `min(logical CPUs, host memory / per-sandbox memory)`.
The CPU term is wrong for this workload twice over. Agent sandboxes spend nearly all
of their life idle — a long conversation punctuated by short bursts of document
generation — so a core-count cap refuses a sandbox that would have consumed no CPU at
all. And it makes capacity depend on a CPU label that lies: a 16-core AMD part with SMT
reports 32 logical CPUs, a 16-core Intel Core Ultra part without SMT reports 16, for
identical memory and near-identical throughput.

**We admit on memory alone.** Memory is what a sandbox consumes from birth to death
whether or not it is doing anything; CPU is consumed only during bursts. Contention
during those bursts is resolved by proportional cgroup weight, not by refusing entry at
the door.

## Consequences

When many sandboxes burst at once they all slow proportionally, rather than some being
queued so that others finish fast. That is the deliberate trade. If "some fast" ever
beats "all slower", the answer is a separate queue for heavy operations — not a return
to a core-count cap, which throttles idle sandboxes to protect against busy ones.

Two things become load-bearing that were previously slack:

- **Per-sandbox memory limits.** Without them, an over-committed host fails as a kernel
  OOM kill that picks its victim by score, not by fault — plausibly killing an idle
  conversation, or Postgres, instead of the sandbox that grew.
- **A host memory reserve.** The old formula divided *total* system memory and got away
  with it only because the CPU term was smaller and won the `min()`. Admission must
  divide what is left after the host, the database, and the browser tier are subtracted.
