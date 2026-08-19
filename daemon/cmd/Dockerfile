# Stage 1: Build Rust daemon
FROM rust:1.87-bookworm AS builder
RUN apt-get update && apt-get install -y musl-tools && rm -rf /var/lib/apt/lists/*
WORKDIR /src
# Copy workspace files.
#
# Cargo.lock is NOT optional here. Without it cargo re-resolves the whole graph
# on every image build, so the image tracks whatever crates.io published most
# recently rather than what was tested — and a transitive crate raising its MSRV
# breaks a build whose own source never changed. That is exactly what happened:
# icu_* 2.3.0 shipped requiring rustc 1.88, and every ix image build started
# failing at dependency resolution while `cargo build` on a developer machine
# (which does read the lockfile) kept passing.
COPY Cargo.toml Cargo.lock ./
COPY crates/ crates/
# Build release binary with musl for static linking.
# --locked makes a stale lockfile a loud error instead of a silent re-resolve.
RUN rustup target add x86_64-unknown-linux-musl && \
    cargo build --locked --release --target x86_64-unknown-linux-musl -p ix-server && \
    cp target/x86_64-unknown-linux-musl/release/ixd /ixd

# Stage 2: Base image
#
# The full per-chat sandbox runtime minus the browser: Python AND Node live
# here so the ix-code REPL works for every advertised language on every tier
# (the execute_code tool offers python/javascript/bash regardless of tier).
FROM ubuntu:24.04 AS base

ENV DEBIAN_FRONTEND=noninteractive
ENV LANG=C.UTF-8

# Runtime system packages (libatomic1 is a Node.js runtime dependency;
# xz-utils unpacks the Node tarball)
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl git jq ripgrep fd-find unzip \
    python3 python3-pip python3-venv \
    libatomic1 xz-utils \
    && rm -rf /var/lib/apt/lists/*

# uv (fast Python package installer — lets agents install packages at runtime)
COPY --from=ghcr.io/astral-sh/uv:0.11.2 /uv /usr/local/bin/uv

# Spreadsheet/tabular data libs baked in: sandboxes run with restricted egress,
# so agents cannot pip-install these at runtime. Without them, xlsx generation
# falls back to hand-rolled OOXML XML that strict parsers reject.
RUN pip3 install --break-system-packages --no-cache-dir "openpyxl==3.1.*" "pandas==2.2.*"

# Node.js — pinned tarball straight from nodejs.org into /usr/local, so
# node/npm/npx land on the default PATH both in Docker AND inside VMs
# (Docker ENV PATH does not survive into a Firecracker VM, and ix-code
# spawns a bare `node` from PATH). An immutable image needs no version
# manager, and GitHub release downloads are unreliable from Docker build
# networks — nodejs.org is the dependable source.
ARG NODE_VERSION=25.1.0
RUN curl -fsSL --retry 3 "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz" \
      | tar -xJ -C /usr/local --strip-components=1 \
    && node --version && npm --version \
    && npm install -g pnpm
ENV PNPM_HOME=/root/.local/share/pnpm
ENV PATH="${PNPM_HOME}:${PNPM_HOME}/bin:${PATH}"

# Rust daemon binary
COPY --from=builder /ixd /usr/local/bin/ixd

WORKDIR /workspace
EXPOSE 8080
CMD ["ixd"]

# Stage 3: Browser image (extends base)
#
# A per-chat sandbox that can ALSO drive a browser: base + Chrome + pinchtab.
FROM base AS browser

# Google Chrome
RUN apt-get update && apt-get install -y --no-install-recommends gnupg && \
    curl -fsSL https://dl.google.com/linux/linux_signing_key.pub | gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg && \
    echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] http://dl.google.com/linux/chrome/deb/ stable main" > /etc/apt/sources.list.d/google-chrome.list && \
    apt-get update && apt-get install -y --no-install-recommends google-chrome-stable && \
    rm -rf /var/lib/apt/lists/*
ENV CHROME_BIN=/usr/bin/google-chrome-stable

# Pinchtab browser bridge
COPY --from=pinchtab/pinchtab:latest /usr/local/bin/pinchtab /usr/local/bin/pinchtab

# Node.js tools (Node itself comes from base)
RUN pnpm add -g playwright-core

# Stage 4: Browser-tier VM image (standalone — deliberately NOT built on base)
#
# This stage is the rootfs for the dedicated Firecracker browser-tier VM in
# shared-browser mode. That VM only ever runs pinchtab in SERVER mode (one
# HTTP server managing many isolated Chrome instances) plus a socat bridge
# from AF_VSOCK:1024 → TCP 127.0.0.1:9867 so the host-side Gateway can reach
# pinchtab over the Firecracker vsock transport.
#
# It therefore needs NO ixd, NO Python, NO Node — just Chrome, pinchtab, and
# the three tools the init script uses:
#   socat    — bridges AF_VSOCK (Firecracker host→guest) to pinchtab's TCP listener
#   wget     — health-polls pinchtab before socat starts
#   iproute2 — `ip link set lo up`; Firecracker guests boot with loopback DOWN,
#              and pinchtab/socat both use 127.0.0.1
#
# The Chrome install block is duplicated from the browser stage on purpose:
# inheriting from `browser` would drag in ~700 MB of sandbox runtime this VM
# never executes, and a standalone stage means base/browser changes no longer
# invalidate this image's build cache.
FROM ubuntu:24.04 AS browser-vm

ENV DEBIAN_FRONTEND=noninteractive
ENV LANG=C.UTF-8

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl gnupg socat wget iproute2 \
    && curl -fsSL https://dl.google.com/linux/linux_signing_key.pub | gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg \
    && echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] http://dl.google.com/linux/chrome/deb/ stable main" > /etc/apt/sources.list.d/google-chrome.list \
    && apt-get update && apt-get install -y --no-install-recommends google-chrome-stable \
    && rm -rf /var/lib/apt/lists/*
ENV CHROME_BIN=/usr/bin/google-chrome-stable

# Pinchtab browser bridge (server mode)
COPY --from=pinchtab/pinchtab:latest /usr/local/bin/pinchtab /usr/local/bin/pinchtab

# Persistent browser-state directory (Chrome profiles / cookies).
# In production this path is backed by a second ext4 block device attached
# via the Firecracker /drives API (see build & verify notes in the plan doc).
RUN mkdir -p /var/lib/ix/browser-state

# Init script — becomes CMD (PID 1 in the VM).
COPY cmd/browser-vm-init.sh /usr/local/bin/browser-vm-init
RUN chmod +x /usr/local/bin/browser-vm-init

CMD ["/usr/local/bin/browser-vm-init"]
