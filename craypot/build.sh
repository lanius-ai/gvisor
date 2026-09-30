#!/bin/bash
# Builds gvisor.tar.zstd in the nightly layout (what Substrate's atelet
# extracts): runsc and gvisor-bin/gvisor_sentry from this branch; the other
# sidecars (checkpointgofer, runsc-metric-server, runsc-fd-parking,
# gvisor-sentry-prewarmer) and containerd-shim-runsc-v1 from the upstream
# nightly pinned below, which is the nightly closest to this branch's base.
# ponytail: the reused sidecars are bazel-only upstream; rebuild them here if
# one ever needs a fork change.
# Usage: craypot/build.sh <out-dir>   (writes <out-dir>/x86_64/gvisor.tar.zstd{,.sha256}
# and leaves the unpacked tree in <out-dir>/x86_64/dist)
set -euo pipefail
NIGHTLY=2026-09-29
NIGHTLY_SHA256=5adec45970c34a6b8f11cb04afcb95c276846621f84412164e83bc539ca16782
cd "$(dirname "$0")/.."
out=$(realpath -m "$1")/x86_64
rm -rf "$out"; mkdir -p "$out/dist"

base=$(git log -1 --format=%s --grep='^Merge release-' | sed -E 's/^Merge (release-[0-9.]+-[0-9]+)-g.*/\1/')
version="$base-craypot-$(git rev-parse --short=9 HEAD)"

curl -fsSLo "$out/nightly.tar.zstd" "https://storage.googleapis.com/gvisor/releases/nightly/$NIGHTLY/x86_64/gvisor.tar.zstd"
echo "$NIGHTLY_SHA256  $out/nightly.tar.zstd" | sha256sum -c --quiet
tar --zstd -xf "$out/nightly.tar.zstd" -C "$out/dist"
rm "$out/nightly.tar.zstd"

export CGO_ENABLED=0 GOOS=linux GOARCH=amd64
go build -trimpath -ldflags "-X gvisor.dev/gvisor/runsc/version.version=$version" -o "$out/dist/runsc" ./runsc
go build -trimpath -ldflags "-X gvisor.dev/gvisor/runsc/version.version=$version" -o "$out/dist/gvisor-bin/gvisor_sentry" ./runsc/cmd/sentry
"$out/dist/runsc" --version | grep -qF "$version"

(cd "$out/dist" && tar --sort=name --owner=0 --group=0 --numeric-owner --mode=u+rwX,go+rX,go-w --mtime='2000-01-01 00:00:00Z' \
  -cf - containerd-shim-runsc-v1 runsc gvisor-bin) | zstd -q -19 -T0 -o "$out/gvisor.tar.zstd"
(cd "$out" && sha256sum gvisor.tar.zstd > gvisor.tar.zstd.sha256)
echo "$version" > "$out/version"
cat "$out/gvisor.tar.zstd.sha256"
