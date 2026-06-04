# The ix Handbook

Everything you need to understand, integrate, and run ix — written to be
readable without deep systems knowledge. Diagrams render directly on GitHub.

| Document | Who it's for | Questions it answers |
|---|---|---|
| [01 — What is ix?](01-what-is-ix.md) | Everyone (no code) | Why does this exist? What can a sandbox do? Why MicroVMs and not Docker? What does it cost to run? |
| [02 — Architecture](02-architecture.md) | Engineers | How do the Go SDK and Rust daemon split the work? What happens when I call `sb.Shell()`? How do pooling, snapshots, and the three networking layers work? |
| [03 — The Browser Subsystem](03-browser.md) | Engineers using browser tools | Why is the browser special? In-VM vs shared tier — which and when? What tools do agents get? What are the known gotchas? |
| [04 — Integration](04-integration.md) | App developers | What is the one interface I code against? What's the minimal wiring? Eager or lazy sandboxes? How would a non-Go app use ix? |
| [05 — Operations](05-operations.md) | Operators / DevOps | What do I install? I changed X — what do I rebuild? How do I deploy the shared browser tier? Why is my sandbox misbehaving? |

## Reading paths

- **Stakeholder / PM** → just [01](01-what-is-ix.md).
- **New engineer on the team** → [01](01-what-is-ix.md) → [02](02-architecture.md) → [03](03-browser.md) → [04](04-integration.md).
- **Integrating ix into an app** → [04](04-integration.md), plus [03](03-browser.md) if you use browser tools.
- **Deploying / on-call** → [01](01-what-is-ix.md) → [05](05-operations.md).

## Keeping these docs honest

Each document carries a `<!-- source-of-truth: … -->` comment naming the code
it was written against. If you change that code, re-check the document. Design
history and work-in-progress plans live separately in
[`docs/superpowers/`](../superpowers/).
