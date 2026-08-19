# ix does not pause or resume sandboxes

Status: accepted

Comparable runtimes (E2B, Daytona) let an idle sandbox be frozen to disk and revived
later, so a user returning to a session finds their processes and loaded state intact.
It is the feature most people expect to find here, and its absence looks like an
oversight.

It is not. Oasis already solves that problem a layer up: it commits an agent's files out
to durable storage after each tool call, and keeps agent state in Postgres and object
storage. A sandbox therefore never holds the only copy of anything. **A sandbox is
either alive or gone** — ix will not implement pause, resume, or memory checkpointing of
user sandboxes.

## Consequences

Durability across a gap is Oasis's job. Reviving a conversation costs a fresh sandbox
plus a re-prefetch of its mounts, and that cost is accepted rather than engineered away.

Building pause/resume anyway would mean two systems owning the same guarantee, with the
usual result: they disagree, and the one nobody tested wins.

The golden snapshot is not an exception to this. It holds a pre-warmed empty template
used to create new sandboxes quickly — never a user's work.
