#!/bin/sh
# Turn ALLOW_HOSTS (comma list; "*.x.com" wildcards) into tinyproxy's regex filter, default-deny.
: > /etc/tinyproxy/allow.txt
for h in $(echo "$ALLOW_HOSTS" | tr ',' ' '); do
  case "$h" in
    \*.*) printf '^[^.]+\\.%s$\n' "$(echo "${h#*.}" | sed 's/\./\\./g')" >> /etc/tinyproxy/allow.txt ;;
    *)    printf '^%s$\n' "$(echo "$h" | sed 's/\./\\./g')" >> /etc/tinyproxy/allow.txt ;;
  esac
done
cat > /etc/tinyproxy/tinyproxy.conf <<CONF
Port 3128
Listen 0.0.0.0
Timeout 600
Allow 0.0.0.0/0
ConnectPort 443
FilterDefaultDeny Yes
Filter "/etc/tinyproxy/allow.txt"
FilterExtended On
LogLevel Info
CONF
exec tinyproxy -d -c /etc/tinyproxy/tinyproxy.conf
