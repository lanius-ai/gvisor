#!/bin/bash
# Release gate for a runsc build with in-sandbox FUSE (AgentFS live workspace).
# runsc runs inside privileged docker containers standing in for nodes
# (systrap, --overlay2=root:self, STRICT sidecars from the tarball layout):
#   work.sh    exec, mmap coherence, stock git, venv/dlopen, SQLite WAL, gcc,
#              cargo, server exit on umount; on agentfs, bindfs, bindfs-kc
#   stress.sh  8 workers of git/dlopen/process churn on agentfs
#   cr         checkpoint with an AgentFS mount (server spawned by the
#              container init, as the craypot launcher does) and a dirty
#              MAP_SHARED page, restore on a fresh node and --root
# Usage: gate.sh <dist-dir> <work-dir>
#   dist-dir  unpacked gvisor.tar.zstd (runsc, gvisor-bin/)
#   work-dir  scratch (bundle, checkpoint image, out-*.txt)
# Needs the rootfs image: docker build -t craypot-gvisor-harness craypot/harness
set -uo pipefail
B=$(realpath "$1"); H=$(realpath -m "$2"); T=$(cd "$(dirname "$0")" && pwd)
IMG=${IMG:-craypot-gvisor-harness}
OUTER=debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a
STRESS_SECS=${STRESS_SECS:-60}
# RESULT lines each run must print; fewer means it died part way.
declare -A EXPECT=([agentfs]=39 [bindfs]=41 [bindfs-kc]=41 [stress]=1 [cr]=12)
R="/b/runsc --root /tmp/runsc --platform=systrap --overlay2=root:self --network=none --ignore-cgroups"

as_root() { docker run --rm -v "$H:/h" "$OUTER" sh -c "$1"; }
node() { docker rm -f "$1" >/dev/null 2>&1; docker run -d --name "$1" --privileged -v "$H:/h" -v "$B:/b:ro" "$OUTER" sleep infinity >/dev/null; }
# x <node> <timeout-s> <cmd...>: runsc exec in sandbox ff; dumps sentry stacks on timeout.
x() {
  local n=$1 t=$2; shift 2
  timeout "$t" docker exec -i "$n" $R exec ff "$@"; local rc=$?
  [ $rc = 124 ] && { echo "TIMEOUT after ${t}s: $*"; docker exec "$n" $R debug --stacks ff > "$H/stacks-$n.txt" 2>&1; }
  return $rc
}
start() { # start <node> <bundle>
  node "$1"
  docker exec "$1" sh -c "$R run -detach -bundle /h/$2 ff </dev/null >/h/run-$1.log 2>&1" || { cat "$H/run-$1.log"; return 1; }
}
stop() { docker rm -f "$@" >/dev/null 2>&1; as_root 'rm -f /h/bundle/rootfs/.gvisor.filestore.*'; }

mkdir -p "$H"; stop gate-node cr-node1 cr-node2
as_root 'rm -rf /h/*'; mkdir -p "$H/bundle/rootfs" "$H/bundle-cr" "$H/ckpt"
cid=$(docker create "$IMG") && docker export "$cid" | docker run --rm -i -v "$H:/h" "$OUTER" tar -xf - -C /h/bundle/rootfs
docker rm "$cid" >/dev/null
cp "$T/config.json" "$H/bundle/config.json"
jq '.process.args = ["/bin/sh", "/opt/t/init.sh"] | .root.path = "/h/bundle/rootfs"' "$T/config.json" > "$H/bundle-cr/config.json"
"$B/runsc" --version

for mode in agentfs bindfs bindfs-kc; do
  echo "== work.sh $mode"
  start gate-node bundle && x gate-node 900 /opt/t/work.sh $mode | tee "$H/out-$mode.txt"
  stop gate-node
done

echo "== stress.sh agentfs ${STRESS_SECS}s"
start gate-node bundle && x gate-node $((STRESS_SECS + 600)) /opt/t/stress.sh agentfs "$STRESS_SECS" > "$H/stress.log" 2>&1
cat "$H/stress.log"
if grep -q '^STRESS-OK' "$H/stress.log" && ! grep -q 'failed' "$H/stress.log"; then r=PASS; else r=FAIL; fi
echo "RESULT stress_agentfs $r" > "$H/out-stress.txt"
stop gate-node

echo "== checkpoint/restore"
(
  start cr-node1 bundle-cr || exit 1
  x cr-node1 120 sh -c 'mkdir -p /var/lib/agentfs /w /run/spawn && cd /var/lib/agentfs && agentfs init ws >/dev/null && echo "exec agentfs mount /var/lib/agentfs/.agentfs/ws.db /w --foreground --system" > /run/spawn/agentfs.sh; for i in $(seq 50); do mountpoint -q /w && break; sleep 0.1; done
    git clone -q /opt/click /w/click && echo pre-ckpt > /w/marker && cp /bin/true /w/true
    rm -f /tmp/cr.ready /tmp/cr.go; echo "exec python3 /opt/t/crhold.py" > /run/spawn/crhold.sh; for i in $(seq 50); do [ -f /tmp/cr.ready ] && break; sleep 0.1; done
    echo "agentfs pid $(pgrep -x agentfs) holder pid $(cat /tmp/cr.ready)"' || exit 2
  t0=$(date +%s%N)
  docker exec cr-node1 sh -c "$R checkpoint -image-path /h/ckpt ff" || exit 3
  t1=$(date +%s%N)
  stop cr-node1
  echo "checkpoint $(( (t1 - t0) / 1000000 )) ms, image $(du -sh "$H/ckpt" | cut -f1)"
  node cr-node2
  t0=$(date +%s%N)
  docker exec cr-node2 sh -c "$R restore -image-path /h/ckpt -bundle /h/bundle-cr -detach ff </dev/null >/h/cr-restore.log 2>&1" || { cat "$H/cr-restore.log"; exit 4; }
  t1=$(date +%s%N)
  echo "restore $(( (t1 - t0) / 1000000 )) ms"
  x cr-node2 300 sh -c 'echo "agentfs pid $(pgrep -x agentfs)"
    r() { if "$@" >/dev/null 2>&1; then echo "RESULT cr.$n PASS"; else echo "RESULT cr.$n FAIL"; fi; }
    n=marker r grep -qx pre-ckpt /w/marker
    n=exec_elf r /w/true
    n=git_status r git -C /w/click status --short
    n=git_commit r sh -c "echo x >> /w/click/README.md && git -C /w/click commit -qam post-restore"
    n=git_fsck r git -C /w/click fsck --no-progress
    touch /tmp/cr.go; for i in $(seq 50); do [ -f /tmp/cr.result ] && break; sleep 0.2; done; cat /tmp/cr.result /run/spawn/crhold.sh.log
    n=umount r umount /w; sleep 1
    if ps -eo stat=,comm= | awk "\$2==\"agentfs\" && \$1 !~ /^Z/" | grep -q .; then echo "RESULT cr.server_exit_after_umount FAIL"; else echo "RESULT cr.server_exit_after_umount PASS"; fi
    n=db_reopens r sh -c "cd /var/lib/agentfs && agentfs fs .agentfs/ws.db cat /marker | grep -qx pre-ckpt"'
) 2>&1 | tee "$H/out-cr.txt"
stop cr-node1 cr-node2

echo "== verdict"
bad=0
for run in "${!EXPECT[@]}"; do
  f="$H/out-$run.txt"
  n=$(grep -c '^RESULT ' "$f" 2>/dev/null); n=${n:-0}
  [ "$n" -ge "${EXPECT[$run]}" ] || { echo "GATE $run: $n of ${EXPECT[$run]} results"; bad=1; }
  while read -r _ name st rest; do
    [ "$st" = PASS ] || { echo "GATE $run: $name $st $rest"; bad=1; }
  done < <(grep '^RESULT ' "$f" 2>/dev/null)
done
[ $bad = 0 ] && echo "GATE PASS" || echo "GATE FAIL"
exit $bad
