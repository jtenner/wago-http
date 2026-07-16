#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
corpus=${CORPORA_DIR:-"$root/testdata/upstream"}/h2spec
address=${H2SPEC_ADDRESS:-127.0.0.1:18080}
host=${address%:*}
port=${address##*:}

if [ ! -f "$corpus/go.mod" ]; then
    printf '%s\n' 'pinned h2spec corpus is absent; run scripts/fetch-test-corpora.sh' >&2
    exit 1
fi

server_log=$(mktemp "${TMPDIR:-/tmp}/wago-http2-h2spec-server-log.XXXXXX")
server_bin=$(mktemp "${TMPDIR:-/tmp}/wago-http2-h2spec-server-bin.XXXXXX")
trap 'kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; rm -f "$server_log" "$server_bin"' EXIT HUP INT TERM

cd "$root"
go build -o "$server_bin" ./cmd/h2spec-server
"$server_bin" -addr "$address" >"$server_log" 2>&1 &
server_pid=$!

sleep 0.1
if ! kill -0 "$server_pid" 2>/dev/null; then
    cat "$server_log" >&2
    exit 1
fi

cd "$corpus"
go run ./cmd/h2spec -h "$host" -p "$port" -S "$@"
