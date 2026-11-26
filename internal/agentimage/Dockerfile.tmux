# A layer over the agent image for the tester, which drives terminal programs through
# tmux. Kept apart so agents that are not testers keep the image they already have.
ARG BASE
FROM ${BASE}
USER root
RUN set -e; \
    if command -v apt-get >/dev/null 2>&1; then \
      apt-get update && apt-get install -y --no-install-recommends tmux && rm -rf /var/lib/apt/lists/*; \
    elif command -v apk >/dev/null 2>&1; then \
      apk add --no-cache tmux; \
    else \
      echo "rig: cannot add tmux to this base image: it has neither apt-get nor apk" >&2; exit 1; \
    fi
