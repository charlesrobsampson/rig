# Spike 04: minimal agent image (toolchain + Claude Code + git).
# certs/corp-ca.pem is the machine's TLS-inspection root CA (git-ignored); an empty
# placeholder works on machines without inspection.
FROM node:22-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates curl procps less ripgrep \
    && rm -rf /var/lib/apt/lists/*
COPY certs/corp-ca.pem /usr/local/share/ca-certificates/corp-ca.crt
RUN update-ca-certificates
ENV NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt
RUN npm install -g @anthropic-ai/claude-code && npm cache clean --force
# the run-time uid is arbitrary (--user), so HOME must be writable by anyone
ENV HOME=/home/agent
RUN mkdir -p /home/agent && chmod 1777 /home/agent
WORKDIR /work
