# Spike 04: agent image = the service's own toolchain image + a Claude Code layer.
# Build with: docker build --build-arg BASE=golang:1.24-bookworm -f overlay.Dockerfile .
ARG BASE=golang:1.24-bookworm
FROM ${BASE}
USER root
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates curl procps ripgrep \
    && rm -rf /var/lib/apt/lists/*
COPY certs/corp-ca.pem /usr/local/share/ca-certificates/corp-ca.crt
RUN update-ca-certificates
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt \
    CURL_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
# native installer: no Node needed in the service's toolchain image
RUN curl -fsSL https://claude.ai/install.sh | bash && cp -L /root/.local/bin/claude /usr/local/bin/claude && chmod 755 /usr/local/bin/claude
ENV HOME=/home/agent
RUN mkdir -p /home/agent && chmod 1777 /home/agent
WORKDIR /work
