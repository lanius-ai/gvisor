#!/bin/sh
# Container init for C/R tests: runs /run/spawn/*.sh as its own children, so
# long-lived processes (FUSE server, mapping holder) are not runsc-exec'd
# (runsc restore does not bring exec'd processes back).
mkdir -p /run/spawn
while true; do
  for f in /run/spawn/*.sh; do
    [ -f "$f" ] || continue
    mv "$f" "$f.run" && (sh "$f.run" > "$f.log" 2>&1 &)
  done
  sleep 0.1
done
