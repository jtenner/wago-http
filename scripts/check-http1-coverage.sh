#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
profile=$(mktemp "${TMPDIR:-/tmp}/wago-http1-coverage.XXXXXX")
trap 'rm -f "$profile"' EXIT HUP INT TERM

cd "$root"
go test ./http -coverprofile="$profile" -count=1

awk '
    $1 ~ /\/http\/parser\.go:/ {
        total += $2
        if ($3 != 0) covered += $2
    }
    END {
        if (total == 0) {
            print "http/parser.go was absent from the coverage profile" > "/dev/stderr"
            exit 1
        }
        percent = 100 * covered / total
        printf "http/parser.go statement coverage: %d/%d (%.2f%%)\n", covered, total, percent
        if (covered * 100 < total * 98) {
            print "HTTP/1 parser coverage is below the required 98%" > "/dev/stderr"
            exit 1
        }
    }
' "$profile"
