#!/usr/bin/env bash
# Spike 05 probe: an agent on a Docker --internal network can reach only the allowlist proxy.
# Needs images: rig-agent-spike (spikes/container) and rig-egress-goproxy (spikes/egress/goproxy).
# Usage: probe.sh [proxy-image]   (default rig-egress-goproxy; see probe-tinyproxy notes in the doc)
set -uo pipefail
PROXY_IMG=${1:-rig-egress-goproxy}
AGENT=rig-agent-spike
INT=rig-egress-int; EXT=rig-egress-ext
cleanup() { docker rm -f egress-proxy egress-svc >/dev/null 2>&1; docker network rm $INT $EXT >/dev/null 2>&1; }
cleanup; trap cleanup EXIT
ok()   { printf '  %-58s %s\n' "$1" "$2"; }
run()  { docker run --rm --network $INT "$@"; }

docker network create --internal $INT >/dev/null
docker network create $EXT >/dev/null
docker run -d --name egress-proxy --network $INT -e ALLOW_HOSTS='api.anthropic.com,*.anthropic.com,localhost' "$PROXY_IMG" >/dev/null
docker network connect $EXT egress-proxy            # only the proxy gets a route out
docker run -d --name egress-svc --network $INT $AGENT node -e "require('http').createServer((q,r)=>r.end('svc-ok')).listen(8080)" >/dev/null
sleep 1.5
P=http://egress-proxy:3128

echo "== containment: agent on the internal network only"
r=$(run $AGENT curl -s -m 5 -o /dev/null -w '%{http_code}' https://example.com 2>&1); ok "direct https://example.com" "code=$r (000 = unreachable)"
r=$(run $AGENT curl -s -m 5 -o /dev/null -w '%{http_code}' http://1.1.1.1 2>&1); ok "direct http://1.1.1.1 (raw IP)" "code=$r (000 = unreachable)"
r=$(run $AGENT getent hosts example.com 2>&1 | head -1); ok "DNS lookup of example.com from agent" "${r:-no answer (no DNS leak)}"

echo "== through the proxy"
r=$(run $AGENT curl -s -m 10 -x $P -o /dev/null -w '%{http_code}' https://api.anthropic.com/); ok "allowed host api.anthropic.com" "HTTP $r (404 from the API = tunnel works)"
r=$(run $AGENT curl -s -m 10 -x $P -o /dev/null -w '%{http_code}' https://example.com/); ok "denied host example.com" "HTTP $r (000 = proxy refused CONNECT)"
r=$(run $AGENT curl -s -m 10 -x $P -o /dev/null -w '%{http_code}' http://api.anthropic.com/); ok "plain http to an allowed host" "HTTP $r"
r=$(run $AGENT curl -s -m 10 -x $P -o /dev/null -w '%{http_code}' https://api.anthropic.com:8443/); ok "allowed host on a non-allowed port 8443" "HTTP $r"
r=$(run $AGENT curl -s -m 10 -x $P -o /dev/null -w '%{http_code}' https://localhost/); ok "allowlisted name that resolves to loopback" "HTTP $r"

echo "== service-to-service stays direct (NO_PROXY)"
r=$(run -e http_proxy=$P -e no_proxy=egress-svc $AGENT curl -s -m 5 http://egress-svc:8080/); ok "http://egress-svc:8080 with NO_PROXY=egress-svc" "$r"
r=$(run -e http_proxy=$P $AGENT curl -s -m 5 http://egress-svc:8080/ | head -c 60); ok "same without NO_PROXY (goes via proxy)" "${r:-<empty>}"

echo "== Claude Code through the proxy (dummy key, so the API answers 401)"
run -e HTTPS_PROXY=$P -e ANTHROPIC_API_KEY=sk-ant-dummy $AGENT sh -c 'timeout 40 claude -p hi 2>&1 | head -2' | sed 's/^/  claude: /'
echo "  hosts the proxy saw:"; docker logs egress-proxy 2>&1 | python3 -c '
import sys,json,collections
c=collections.Counter()
for l in sys.stdin:
    try: d=json.loads(l)
    except Exception: continue
    if d.get("action") in("allow","deny"): c[(d["action"],d["host"])]+=1
for (a,h),n in sorted(c.items()): print("   ",a.ljust(5),h,"x"+str(n))'
echo "== Claude Code WITHOUT the proxy env (must fail to connect)"
run -e ANTHROPIC_API_KEY=sk-ant-dummy $AGENT sh -c 'timeout 40 claude -p hi 2>&1 | head -2' | sed 's/^/  claude: /'
