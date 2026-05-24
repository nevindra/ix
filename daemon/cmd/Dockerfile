# Stage 1: Build Rust daemon
FROM rust:1.87-bookworm AS builder
RUN apt-get update && apt-get install -y musl-tools && rm -rf /var/lib/apt/lists/*
WORKDIR /src
# Copy workspace files
COPY Cargo.toml ./
COPY crates/ crates/
# Build release binary with musl for static linking
RUN rustup target add x86_64-unknown-linux-musl && \
    cargo build --release --target x86_64-unknown-linux-musl -p ix-server && \
    cp target/x86_64-unknown-linux-musl/release/ixd /ixd

# Stage 2: Base image
FROM ubuntu:24.04 AS base

ENV DEBIAN_FRONTEND=noninteractive
ENV LANG=C.UTF-8

# System packages
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl git jq ripgrep fd-find unzip \
    python3 python3-pip python3-venv \
    gcc python3-dev libffi-dev libjpeg-dev libpng-dev libcairo2-dev pkg-config \
    && rm -rf /var/lib/apt/lists/*

# uv (fast Python package installer)
COPY --from=ghcr.io/astral-sh/uv:0.11.2 /uv /usr/local/bin/uv

# Jupyter kernel for code execution
RUN uv pip install --system --break-system-packages ipykernel

# Rust daemon binary
COPY --from=builder /ixd /usr/local/bin/ixd

WORKDIR /workspace
EXPOSE 8080
CMD ["ixd"]

# Stage 3: Browser image (extends base)
FROM base AS browser

# Node.js via fnm
ENV FNM_DIR=/root/.local/share/fnm
ENV PATH="/root/.local/share/fnm:${PATH}"
RUN curl -fsSL https://fnm.vercel.app/install | bash -s -- --install-dir /root/.local/share/fnm --skip-shell && \
    eval "$(fnm env)" && \
    fnm install 25 && \
    fnm default 25 && \
    corepack enable pnpm
ENV PATH="/root/.local/share/fnm/aliases/default/bin:${PATH}"
ENV PNPM_HOME=/root/.local/share/pnpm
ENV PATH="${PNPM_HOME}:${PATH}"

# Google Chrome
RUN curl -fsSL https://dl.google.com/linux/linux_signing_key.pub | gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg && \
    echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] http://dl.google.com/linux/chrome/deb/ stable main" > /etc/apt/sources.list.d/google-chrome.list && \
    apt-get update && apt-get install -y --no-install-recommends google-chrome-stable && \
    rm -rf /var/lib/apt/lists/*
ENV CHROME_BIN=/usr/bin/google-chrome-stable

# Pinchtab browser bridge
COPY --from=pinchtab/pinchtab:latest /pinchtab /usr/local/bin/pinchtab

# Node.js tools
RUN pnpm add -g playwright-core

# Stage 4: Full image (extends browser)
FROM browser AS full

# Scientific Python packages
RUN uv pip install --system --break-system-packages \
    numpy pandas matplotlib seaborn Pillow \
    python-docx openpyxl pypdf jinja2 cairosvg

# Node.js document/chart tools
RUN pnpm add -g \
    pptxgenjs chartjs-node-canvas @mermaid-js/mermaid-cli sharp papaparse
