FROM rust:1.88-bookworm AS builder
WORKDIR /build
COPY Cargo.toml Cargo.lock ./
COPY src/ src/
RUN cargo build --release --features nats

FROM node:22-bookworm-slim

# Install Claude Code CLI
RUN npm install -g @anthropic-ai/claude-code

# Copy codewire binary
COPY --from=builder /build/target/release/cw /usr/local/bin/cw

# Create non-root user (Claude Code refuses --dangerously-skip-permissions as root)
RUN useradd -m -s /bin/bash codewire
USER codewire
WORKDIR /home/codewire

ENV TERM=xterm-256color
ENV HOME=/home/codewire

ENTRYPOINT ["cw"]
CMD ["daemon"]
