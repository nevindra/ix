# ix

The sandbox runtime that Oasis agents run code inside. ix owns *where* code runs and
*how much of the host it may take*. It does not own the durability of anything the
agent produces — that belongs to Oasis.

## Language

### Lifecycle

**Sandbox**:
One microVM that runs agent code for one conversation. Disposable by design: nothing
inside it is expected to outlive the session, and nothing inside it is the only copy
of anything.
_Avoid_: container, VM, instance, environment

**Pool**:
Sandboxes created ahead of demand and held unclaimed, so a conversation that needs one
does not wait for a birth.
_Avoid_: cache, warm pool, reserve

**Golden Snapshot**:
A memory image of one pre-warmed template sandbox, used to create new sandboxes without
booting them. It always holds an empty template, never a user's work.
_Avoid_: checkpoint, savepoint, image, backup

**Eviction**:
Destroying a sandbox that has no work in flight and whose idle window has passed, to
free its slot for a waiting conversation.
_Avoid_: eviction of live sandboxes — a sandbox with work in flight is never a candidate

### Capacity

**Slot**:
One permit to hold a live sandbox. Slots are scarce because host memory is scarce; they
are not scarce because cores are scarce.
_Avoid_: seat, license, concurrency unit

**Admission**:
The decision to grant a slot, refuse it, or make the caller wait. Decided on memory
alone, because memory is what a sandbox consumes from birth to death whether or not it
is doing anything.

**Contention**:
Several sandboxes wanting CPU at the same moment. Resolved by sharing cores
proportionally, not by refusing admission — a sandbox that is merely idle should never
cost another conversation its slot.

**Burst**:
The short window where a sandbox actually consumes CPU and memory — generating a
document, running a query, rendering a chart. The rest of a sandbox's life is waiting.

### Isolation

**Scratch Disk**:
The private disk where everything a sandbox writes lands. The root filesystem itself is
shared and read-only across all sandboxes.

**Egress Policy**:
The rule set naming which destinations a sandbox may reach over the network.
_Avoid_: firewall, ACL, allowlist (the policy may be either allow- or deny-shaped)

**Browser Tier**:
A single separate VM that all sandboxes share for browser work, rather than each
sandbox carrying its own browser.

## Terms that belong to Oasis, not ix

These name real concepts, but they mean something specific in Oasis and must not be
reused for ix-level ideas:

**Workspace**, **Commit**, **Transaction**, **Change**:
Oasis vocabulary for moving an agent's files out to durable backends. They describe
files reaching storage — never a sandbox's memory reaching disk.

**Fork**, **Branch**:
Not ix concepts. ix does not split a running sandbox into several. Speculative agent
work, if it ever exists, is Oasis creating more sandboxes — not ix duplicating one.

**Pause**, **Resume**:
Not ix concepts. A sandbox is either alive or gone. Durability across a gap is Oasis's
job, through its own storage.
