#!/usr/bin/env bash
# Spike 02 probe: spawn a pane, observe state, inject text, close it.
# Uses a private Herdr session so the default session is never touched.
# Usage: spikes/herdr/probe.sh          (then, in another terminal, watch with:
#        herdr session attach rig-spike)
set -euo pipefail

SESSION=rig-spike
h() { herdr --session "$SESSION" "$@"; }
json() { python3 -c "import sys,json; d=json.load(sys.stdin); $1"; }

if ! herdr session list | grep -q "^$SESSION .*running"; then
  (herdr --session "$SESSION" server >/dev/null 2>&1 &)
  sleep 2
fi

dir=$(mktemp -d)
trap 'h workspace close "$ws" >/dev/null 2>&1 || true; rm -rf "$dir"' EXIT

echo "1. spawn workspace + shell pane (cwd=$dir)"
out=$(h workspace create --cwd "$dir" --label rig-probe --no-focus)
ws=$(echo "$out" | json 'print(d["result"]["workspace"]["workspace_id"])')
pane=$(echo "$out" | json 'print(d["result"]["root_pane"]["pane_id"])')
echo "   workspace=$ws pane=$pane"

echo "2. run a command that reads stdin into a file"
h pane run "$pane" "cat > $dir/received.txt" >/dev/null
sleep 0.5

echo "3. inject multiline text (quotes, \$vars, tab) then Enter + Ctrl-D"
h pane send-text "$pane" $'line one "quoted" $HOME\nline two\ttabbed' >/dev/null
h pane send-keys "$pane" Enter >/dev/null
h pane send-keys "$pane" ctrl+d >/dev/null
sleep 1
echo "   bytes received:"; od -c "$dir/received.txt" | sed 's/^/   /'

echo "4. report agent state from outside (label must be a known kind, e.g. claude)"
p2=$(h tab create --workspace "$ws" --cwd "$dir" --label state --no-focus | json 'print(d["result"]["root_pane"]["pane_id"])')
h pane run "$p2" "sleep 30" >/dev/null; sleep 0.5
for s in working blocked idle; do
  h pane report-agent "$p2" --source rig --agent claude --state "$s" >/dev/null
  echo "   reported $s -> herdr says: $(h agent get "$p2" | json 'print(d["result"]["agent"]["agent_status"])')"
done

echo "5. wait for a state. A reported idle that no human has seen reads as done, so wait on both."
h agent wait "$p2" --until idle --until done --timeout 2000 | json 'print("   waited ok:", d["result"]["agent"]["agent_status"])'

echo "6. close the pane; later calls return pane_not_found"
h pane close "$p2" >/dev/null
h pane get "$p2" 2>&1 | json 'print("   after close:", d.get("error",{}).get("code"))'
echo done
