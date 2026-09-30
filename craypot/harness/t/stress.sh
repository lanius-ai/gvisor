#!/bin/bash
# Concurrency stress on an in-sandbox FUSE mount: parallel stock-git, dlopen,
# sqlite WAL and process churn. Usage: stress.sh <mode> <seconds>
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin HOME=/root
MODE=$1; SECS=${2:-60}; W=/w
umount -l $W 2>/dev/null; rm -rf $W /srv/b /var/lib/agentfs; mkdir -p $W /srv/b
case $MODE in
bindfs) bindfs /srv/b $W ;;
rootfs) ;;
agentfs) mkdir -p /var/lib/agentfs && cd /var/lib/agentfs && agentfs init ws >/dev/null && cd / \
  && (agentfs mount /var/lib/agentfs/.agentfs/ws.db $W --foreground --system >/var/log/agentfs.log 2>&1 &) \
  && for i in $(seq 50); do mountpoint -q $W && break; sleep 0.1; done ;;
esac
git clone -q /opt/click $W/click || exit 1
for w in 1 2 3 4 5 6 7 8; do git clone -q /opt/click $W/c$w || exit 1; done
python3 -m venv $W/venv || exit 1
end=$((SECONDS + SECS))
worker() {
  local k=0
  while [ $SECONDS -lt $end ]; do
    case $(( (k + $1) % 5 )) in
    0) git -C $W/click status --short >/dev/null && git -C $W/click log --oneline -20 >/dev/null ;;
    1) echo "$1 $k" >> $W/c$1/f && git -C $W/c$1 add -A && git -C $W/c$1 commit -qm "w$1 $k" ;;
    2) git -C $W/c$1 gc -q && git -C $W/c$1 fsck --no-progress >/dev/null ;;
    3) $W/venv/bin/python -c 'import json, sqlite3, ctypes; ctypes.CDLL(None)' ;;
    4) for j in 1 2 3 4 5; do sh -c true; done; ps -e >/dev/null ;;
    esac || { echo "worker $1 op $(( (k + $1) % 5 )) failed"; }
    k=$((k + 1))
  done
  echo "worker $1 done $k ops"
}
for w in 1 2 3 4 5 6 7 8; do worker $w & done
wait
ok=1; for w in 1 2 3 4 5 6 7 8; do git -C $W/c$w fsck --no-progress || ok=0; done
[ $ok = 1 ] && echo "STRESS-OK fsck"
