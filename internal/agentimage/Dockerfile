# The agent image: the service's own toolchain image plus what an agent needs
# (docs/spikes/04-agent-container.md §1). Built by rig; the base comes in as BASE.
ARG BASE=debian:bookworm-slim
FROM ${BASE}
USER root
# Extra certificates go in before anything is downloaded: behind a TLS-inspecting proxy
# even a package index fails without them. (Debian's apt uses http and the bundle below
# does not exist yet there; update-ca-certificates after the install covers it.)
COPY certs/ /usr/local/share/ca-certificates/
RUN for f in /usr/local/share/ca-certificates/*.crt; do \
      [ -e "$f" ] && [ -f /etc/ssl/certs/ca-certificates.crt ] && cat "$f" >> /etc/ssl/certs/ca-certificates.crt; \
    done; true
RUN set -e; \
    if command -v apt-get >/dev/null 2>&1; then \
      apt-get update && apt-get install -y --no-install-recommends git ca-certificates curl procps ripgrep bash \
      && rm -rf /var/lib/apt/lists/*; \
    elif command -v apk >/dev/null 2>&1; then \
      apk add --no-cache git ca-certificates curl procps ripgrep bash libgcc libstdc++; \
    else \
      echo "rig: cannot add git and curl to this base image: it has neither apt-get nor apk" >&2; exit 1; \
    fi
RUN update-ca-certificates
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt \
    NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt \
    CURL_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
# The native installer needs no Node in the service's image. A base that already has
# claude keeps its own.
ARG CLAUDE_VERSION=latest
RUN command -v claude >/dev/null 2>&1 || ( \
      curl -fsSL https://claude.ai/install.sh | bash -s "${CLAUDE_VERSION}" \
      && cp -L /root/.local/bin/claude /usr/local/bin/claude && chmod 755 /usr/local/bin/claude )
# Any uid can run here: rig starts the container as the host user, who has no home.
ENV HOME=/home/agent
RUN mkdir -p /home/agent && chmod 1777 /home/agent
WORKDIR /work
