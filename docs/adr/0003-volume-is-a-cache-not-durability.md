# A Volume is a cache, not durability

Status: proposed

ADR 0002 says a sandbox is either alive or gone, and that durability across a gap is
Oasis's job. That holds for an agent's *outputs*: Oasis commits `/workspace` out after
every tool call, so a sandbox never holds the only copy of anything.

It does not hold for an agent's *inputs* when the input is a git repository. A code-review
run needs a checkout with its history and its dependency tree. Cloning that into a fresh
sandbox on every run is the dominant cost of the run, and pushing it through Oasis's
per-file commit path is worse: a `.git` objects directory or a `node_modules` tree becomes
thousands of `files` rows, blobs, and version chains, which is exactly the incident Athena
already had with `node_modules`.

**A Volume is a host-local block device that outlives the sandbox it is attached to.** It
is a private sparse ext4 file, attached read-write to one sandbox at a time, mounted at a
path outside `/workspace` so Oasis's mount layer never walks it.

**A Volume is a cache.** The durable copy of its contents is the git remote. If the host
dies, the Volume is gone and the next run clones again. ix never uploads a Volume
anywhere, never backs it up, and evicts it under disk pressure.

This does not reopen ADR 0002. The sandbox is still alive or gone; nothing about its
memory or processes is checkpointed. Only a block device it borrowed survives it, and that
device holds nothing that cannot be rebuilt from outside.

## Consequences

- ix does not name this a Workspace. That word belongs to Oasis (see CONTEXT.md), and a
  Volume is precisely the thing Oasis's Workspace must not see.
- A Volume is single-writer. Two sandboxes asking for the same Volume at once is an error
  the caller resolves, not a queue ix maintains.
- Because it is a cache, ix may delete an unattached Volume when the host is short of
  disk. Callers must treat "the repo is already there" as an optimisation to check for,
  never as a guarantee.
- A sandbox with a Volume boots cold, not from the golden snapshot: a restored VM cannot
  take a drive the snapshot did not have. The ~1 s cold boot is accepted; it is nothing
  against the clone it replaces.
- Credentials used to fill a Volume must never be written to it. The Volume is reattached
  to later sandboxes with different callers and different tokens.
