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

# Runtime system packages
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl git jq ripgrep fd-find unzip \
    python3 python3-pip python3-venv \
    && rm -rf /var/lib/apt/lists/*

# uv (fast Python package installer, used by the full stage)
COPY --from=ghcr.io/astral-sh/uv:0.11.2 /uv /usr/local/bin/uv

# Rust daemon binary
COPY --from=builder /ixd /usr/local/bin/ixd

WORKDIR /workspace
EXPOSE 8080
CMD ["ixd"]

# Stage 3: Browser image (extends base)
FROM base AS browser

# Node.js runtime needs libatomic1 (previously pulled in transitively by gcc)
RUN apt-get update && apt-get install -y --no-install-recommends \
        libatomic1 \
    && rm -rf /var/lib/apt/lists/*

# Node.js via fnm
ENV FNM_DIR=/root/.local/share/fnm
ENV PATH="/root/.local/share/fnm:${PATH}"
RUN curl -fsSL https://fnm.vercel.app/install | bash -s -- --install-dir /root/.local/share/fnm --skip-shell && \
    eval "$(fnm env)" && \
    fnm install 25 && \
    fnm default 25 && \
    export PATH="/root/.local/share/fnm/aliases/default/bin:$PATH" && \
    npm install -g pnpm
ENV PATH="/root/.local/share/fnm/aliases/default/bin:${PATH}"
ENV PNPM_HOME=/root/.local/share/pnpm
ENV PATH="${PNPM_HOME}:${PNPM_HOME}/bin:${PATH}"

# Google Chrome
RUN apt-get update && apt-get install -y --no-install-recommends gnupg && \
    curl -fsSL https://dl.google.com/linux/linux_signing_key.pub | gpg --dearmor -o /usr/share/keyrings/google-chrome.gpg && \
    echo "deb [arch=amd64 signed-by=/usr/share/keyrings/google-chrome.gpg] http://dl.google.com/linux/chrome/deb/ stable main" > /etc/apt/sources.list.d/google-chrome.list && \
    apt-get update && apt-get install -y --no-install-recommends google-chrome-stable && \
    rm -rf /var/lib/apt/lists/*
ENV CHROME_BIN=/usr/bin/google-chrome-stable

# Pinchtab browser bridge
COPY --from=pinchtab/pinchtab:latest /usr/local/bin/pinchtab /usr/local/bin/pinchtab

# Node.js tools
RUN pnpm add -g playwright-core

# Stage 4: Full image (extends browser)
FROM browser AS full

# Scientific Python packages (build deps installed and purged in same layer;
# libcairo2 is installed explicitly so cairosvg can dlopen it at runtime)
RUN apt-get update && apt-get install -y --no-install-recommends \
        gcc python3-dev libffi-dev libjpeg-dev libpng-dev libcairo2-dev pkg-config \
        libcairo2 \
    && uv pip install --system --break-system-packages \
        numpy pandas matplotlib seaborn Pillow \
        python-docx openpyxl pypdf jinja2 cairosvg \
    && apt-get purge -y gcc python3-dev libffi-dev libjpeg-dev libpng-dev libcairo2-dev pkg-config \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*
