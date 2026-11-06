#!/usr/bin/env bash
# Spike 03 probe (no Claude, no tokens): tmux spawn, capture, death status, injection.
# Also writes spikes/tmux/hooks-settings.json, the per-launch Claude Code hooks
# config (passed with `claude --settings`) that rig would use for state reporting.
set -euo pipefail

SOCK=rig-spike-probe
t() { tmux -L "$SOCK" "$@"; }
dir=$(mktemp -d)
trap 't kill-server 2>/dev/null || true; rm -rf "$dir"' EXIT

echo "1. private server, window with cwd + env + command"
t new-session -d -s tkt -n svc -c "$dir" -x 200 -y 50
t set-option -g remain-on-exit on          # keep dead panes so exit status is readable
t new-window -d -t tkt -n api -c "$dir" -e FOO=bar 'echo cwd=$PWD foo=$FOO; sleep 30'
sleep 0.5
t capture-pane -p -t tkt:api | grep cwd= | sed 's/^/   /'

echo "2. pane death + exit status"
t new-window -d -t tkt -n dies 'exit 3'; sleep 0.5
t list-panes -t tkt:dies -F '   dead=#{pane_dead} exit_status=#{pane_dead_status}'

echo "3. inject multiline text into a cat pane (send-keys -l), check bytes"
t new-window -d -t tkt -n cat "cat > $dir/in.txt"; sleep 0.5
t send-keys -t tkt:cat -l $'line one "q" $HOME `bt`\nline two\ttab \\n end'
t send-keys -t tkt:cat Enter; t send-keys -t tkt:cat C-d; sleep 0.5
od -c "$dir/in.txt" | sed 's/^/   /'

echo "4. inject via bracketed paste (paste-buffer -p), same check"
t new-window -d -t tkt -n cat2 "cat > $dir/in2.txt"; sleep 0.5
printf 'multi\nline "paste"\n' > "$dir/msg.txt"
t load-buffer -b rigmsg "$dir/msg.txt"; t paste-buffer -p -d -b rigmsg -t tkt:cat2
t send-keys -t tkt:cat2 C-d; sleep 0.5
od -c "$dir/in2.txt" | sed 's/^/   /'

echo "5. write the Claude Code hooks settings rig would pass with --settings"
cat > "$(dirname "$0")/hooks-settings.json" <<'JSON'
{
  "hooks": {
    "SessionStart":      [{"hooks": [{"type": "command", "command": "rig agent-event ready"}]}],
    "UserPromptSubmit":  [{"hooks": [{"type": "command", "command": "rig agent-event working"}]}],
    "PreToolUse":        [{"matcher": "*", "hooks": [{"type": "command", "command": "rig agent-event working"}]}],
    "PermissionRequest": [{"matcher": "*", "hooks": [{"type": "command", "command": "rig agent-event blocked"}]}],
    "Stop":              [{"hooks": [{"type": "command", "command": "rig agent-event idle"}]}],
    "SessionEnd":        [{"hooks": [{"type": "command", "command": "rig agent-event exited"}]}]
  }
}
JSON
echo "   wrote spikes/tmux/hooks-settings.json"
echo done
