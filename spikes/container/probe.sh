#!/usr/bin/env bash
# Spike 04 probe (no Claude login needed): ownership, git-in-container, mounts, TTY attach.
# Requires: docker, and the image from `docker build -t rig-agent-spike spikes/container`.
set -uo pipefail
IMG=rig-agent-spike
W=$(cd "$(mktemp -d)" && pwd -P); trap 'rm -rf "$W"' EXIT
ME="$(id -u):$(id -g)"
export GIT_CONFIG_GLOBAL=/dev/null GIT_AUTHOR_NAME=host GIT_AUTHOR_EMAIL=h@x GIT_COMMITTER_NAME=host GIT_COMMITTER_EMAIL=h@x

echo "== setup: main repo + worktree on host"
git init -q -b main "$W/repo" && git -C "$W/repo" commit -q --allow-empty -m init
echo hi > "$W/repo/a.txt" && git -C "$W/repo" add a.txt && git -C "$W/repo" commit -q -m a
git -C "$W/repo" worktree add -q -b feat "$W/wt"
cat "$W/wt/.git"

echo; echo "== 1. file ownership on the host for files created in the container"
for u in "root" "$ME" "4242:4242"; do
  docker run --rm --user "$u" -v "$W/wt:/work" $IMG sh -c 'touch /work/own-$(id -u).txt'
done
ls -ln "$W/wt"/own-*.txt | awk '{print "  ", $3":"$4, $NF}' | sed "s#$W/##"

echo; echo "== 2. git inside container, worktree mounted ONLY (expect failure)"
docker run --rm --user "$ME" -v "$W/wt:/work" $IMG git status 2>&1 | head -3 | sed 's/^/  /'

echo; echo "== 3. worktree + main .git mounted at the same host path"
echo "  -- 3a. worktree mounted at /work (different path than the host)"
docker run --rm --user "$ME" -v "$W/wt:/work" -v "$W/repo/.git:$W/repo/.git" \
  -e GIT_AUTHOR_NAME=agent -e GIT_AUTHOR_EMAIL=a@x -e GIT_COMMITTER_NAME=agent -e GIT_COMMITTER_EMAIL=a@x \
  $IMG sh -c 'cd /work && git status -s | head -3; echo x > b.txt && git add b.txt && git commit -q -m "from container /work" && git log --oneline -1' 2>&1 | sed 's/^/    /'
echo "  -- 3b. worktree mounted at its host path"
docker run --rm --user "$ME" -v "$W/wt:$W/wt" -w "$W/wt" -v "$W/repo/.git:$W/repo/.git" \
  -e GIT_AUTHOR_NAME=agent -e GIT_AUTHOR_EMAIL=a@x -e GIT_COMMITTER_NAME=agent -e GIT_COMMITTER_EMAIL=a@x \
  $IMG sh -c 'echo x2 > b2.txt && git add b2.txt && git commit -q -m "from container same path" && git log --oneline -1' 2>&1 | sed 's/^/    /'
echo "  host sees branch feat at: $(git -C "$W/repo" log --oneline -1 feat)"

echo; echo "== 4. same, but hooks/ and config mounted read-only over the rw .git"
mkdir -p "$W/repo/.git/hooks"
docker run --rm --user "$ME" -v "$W/wt:/work" -v "$W/repo/.git:$W/repo/.git" \
  -v "$W/repo/.git/hooks:$W/repo/.git/hooks:ro" -v "$W/repo/.git/config:$W/repo/.git/config:ro" \
  -e GIT_AUTHOR_NAME=agent -e GIT_AUTHOR_EMAIL=a@x -e GIT_COMMITTER_NAME=agent -e GIT_COMMITTER_EMAIL=a@x \
  $IMG sh -c 'cd /work && echo y > c.txt && git add c.txt && git commit -q -m "ro hooks" && echo "  commit ok";
              printf "#!/bin/sh\ntouch /tmp/pwned\n" > '"$W"'/repo/.git/hooks/post-commit 2>&1 | sed "s/^/  hooks write: /";
              git config user.name evil 2>&1 | sed "s/^/  config write: /"'

echo; echo "== 5. bind-mount speed: create 3000 small files"
t0=$(python3 -c 'import time;print(time.time())')
docker run --rm --user "$ME" -v "$W/wt:/work" $IMG sh -c 'mkdir -p /work/many && cd /work/many && i=0; while [ $i -lt 3000 ]; do echo $i > f$i; i=$((i+1)); done'
t1=$(python3 -c 'import time;print(time.time())'); echo "  via bind mount: $(python3 -c "print(round($t1-$t0,2))")s (includes ~0.5s container start)"
t0=$(python3 -c 'import time;print(time.time())'); mkdir -p "$W/hostmany" && ( cd "$W/hostmany" && i=0; while [ $i -lt 3000 ]; do echo $i > f$i; i=$((i+1)); done )
t1=$(python3 -c 'import time;print(time.time())'); echo "  on host:        $(python3 -c "print(round($t1-$t0,2))")s"

echo; echo "== 6. cold start of an idle container (create+start+exec+rm)"
for i in 1 2 3; do
  t0=$(python3 -c 'import time;print(time.time())'); docker run --rm $IMG true; t1=$(python3 -c 'import time;print(time.time())')
  echo "  run $i: $(python3 -c "print(round($t1-$t0,2))")s"
done

echo; echo "== 7. alternative: isolated shared clone (own .git; main objects mounted read-only)"
git clone -q --shared "$W/repo" "$W/clone" && git -C "$W/clone" checkout -q -b feat2
cat "$W/clone/.git/objects/info/alternates"
du -sk "$W/clone/.git" | awk '{print "  clone .git size (KB):", $1}'
docker run --rm --user "$ME" -v "$W/clone:/work" -v "$W/repo/.git/objects:$W/repo/.git/objects:ro" \
  -e GIT_AUTHOR_NAME=agent -e GIT_AUTHOR_EMAIL=a@x -e GIT_COMMITTER_NAME=agent -e GIT_COMMITTER_EMAIL=a@x \
  $IMG sh -c 'cd /work && git log --oneline -2 && echo z > z.txt && git add z.txt && git commit -q -m "from isolated clone" && git log --oneline -1
              echo "  try to touch main repo refs:"; ls '"$W"'/repo/.git/refs 2>&1 | head -1
              git -C /work fetch 2>&1 | head -1' 2>&1 | sed 's/^/  /'
echo "  host: main branch feat2 exists in main repo? $(git -C "$W/repo" branch --list feat2 | wc -l | tr -d ' ') (0 = isolated)"
git -C "$W/repo" fetch -q "$W/clone" feat2:feat2-from-clone && echo "  host imported it: $(git -C "$W/repo" log --oneline -1 feat2-from-clone)"
