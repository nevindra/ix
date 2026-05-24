# oasis-sandbox-ix

Docker-backed sandbox implementation for [Oasis](https://github.com/nevindra/oasis), the Go AI agent framework.

This module provides:

- **`ix` package** — a `sandbox.Sandbox` implementation that manages Docker
  containers and proxies operations to the `ix` daemon running inside them.
- **`internal/daemon`** — the Go HTTP daemon (`ix` binary) that runs inside
  the container and serves shell execution, code execution, file operations,
  HTTP fetch, web search, and a browser bridge over REST + SSE.
- **`cmd/ix`** — the daemon's `main` entry point and the Dockerfile that
  produces the published runtime image.

## Install

```bash
go get github.com/nevindra/oasis-sandbox-ix
```

## Use

```go
package main

import (
	"context"

	"github.com/nevindra/oasis"
	"github.com/nevindra/oasis/sandbox"
	ix "github.com/nevindra/oasis-sandbox-ix"
)

func main() {
	ctx := context.Background()

	mgr, err := ix.NewManager(ctx, ix.ManagerConfig{
		Image: "ghcr.io/nevindra/oasis-sandbox-ix:latest",
	})
	if err != nil {
		panic(err)
	}
	defer mgr.Close()

	sb, err := mgr.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		panic(err)
	}
	defer mgr.Destroy(ctx, sb)

	ag := oasis.Spawn("coder",
		oasis.WithTools(sandbox.Tools(sb)...),
	)
	_ = ag
}
```

## Image

The runtime image is published to GitHub Container Registry:

```
ghcr.io/nevindra/oasis-sandbox-ix:latest
```

It bundles Python 3, Node.js 25, Chrome, Pinchtab, `ripgrep`/`fd`, and the
document-generation toolchain (matplotlib, pandas, python-docx, openpyxl,
pypdf, Playwright, PptxGenJS, mermaid-cli).

Run with `--shm-size=2g` so Chrome has enough shared memory:

```bash
docker run --shm-size=2g -p 8080:8080 ghcr.io/nevindra/oasis-sandbox-ix:latest
```

## Migration from `github.com/nevindra/oasis/sandbox/ix`

The implementation previously lived in the Oasis repo at
`github.com/nevindra/oasis/sandbox/ix`. The `Sandbox` interface, DTOs, and
`sandbox.Tools()` remain in Oasis; only this implementation has moved.

To migrate:

```diff
-import "github.com/nevindra/oasis/sandbox/ix"
+import ix "github.com/nevindra/oasis-sandbox-ix"
```

Construction call sites and the `ManagerConfig` shape are unchanged.

## License
