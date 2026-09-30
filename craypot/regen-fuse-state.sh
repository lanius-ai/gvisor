#!/bin/bash
# Regenerates pkg/sentry/fsimpl/fuse/fuse_state_autogen.go, which bazel
# generates upstream and the go branch commits. Run it after changing any
# stateify type in the fuse package; CI fails if the file is stale.
# go_stateify is built from a pinned master commit (the go branch lacks it).
set -euo pipefail
MASTER=5f20848ea45a679f1de95417a62cde6e4974ed00
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
git -C "$tmp" init -q
git -C "$tmp" fetch -q --depth 1 --filter=blob:none https://github.com/google/gvisor "$MASTER"
git -C "$tmp" sparse-checkout set --no-cone /go.mod /go.sum /tools/go_stateify/ /tools/constraintutil/
git -C "$tmp" checkout -q FETCH_HEAD
(cd "$tmp" && go build -o "$tmp/go_stateify" ./tools/go_stateify)
cd "$root/pkg/sentry/fsimpl/fuse"
srcs=$(ls *.go | grep -v -e _test.go -e _autogen.go -e _unsafe.go)
"$tmp/go_stateify" -output="$tmp/out.go" -fullpkg=pkg/sentry/fsimpl/fuse -statepkg=gvisor.dev/gvisor/pkg/state -- $srcs
gofmt "$tmp/out.go" > fuse_state_autogen.go
