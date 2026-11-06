# Alpine's tinyproxy package failed to install on Docker Desktop (apk "Permission denied"
# in the package unpack step), so this uses Debian's package instead.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends tinyproxy-bin \
    && rm -rf /var/lib/apt/lists/* && mkdir -p /etc/tinyproxy /var/log/tinyproxy
COPY entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
