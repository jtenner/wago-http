#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
profile=$(mktemp "${TMPDIR:-/tmp}/wago-http2-coverage.XXXXXX")
trap 'rm -f "$profile"' EXIT HUP INT TERM

cd "$root"
go test ./http2 ./http2/request ./http2/server -coverprofile="$profile" -count=1

check() {
    pattern=$1
    label=$2
    minimum=$3
    awk -v pattern="$pattern" -v label="$label" -v minimum="$minimum" '
        index($1, pattern) != 0 {
            total += $2
            if ($3 != 0) covered += $2
        }
        END {
            if (total == 0) {
                print label " was absent from the coverage profile" > "/dev/stderr"
                exit 1
            }
            percent = 100 * covered / total
            printf "%s statement coverage: %d/%d (%.2f%%)\n", label, covered, total, percent
            if (covered * 100 < total * minimum) {
                printf "%s coverage is below the required %d%%\n", label, minimum > "/dev/stderr"
                exit 1
            }
        }
    ' "$profile"
}

check '/http2/frame.go:' 'http2/frame.go' 100
check '/http2/hpack.go:' 'http2/hpack.go' 98
check '/http2/request/request.go:' 'http2/request/request.go' 97
check '/http2/session.go:' 'http2/session.go' 83
check '/http2/abi.go:' 'http2/abi.go' 85
check '/http2/request/transport.go:' 'http2/request/transport.go' 86
check '/http2/server/server.go:' 'http2/server/server.go' 91
