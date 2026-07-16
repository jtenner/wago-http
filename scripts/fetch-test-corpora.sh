#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
lock="$root/testdata/corpora.lock"
destination=${CORPORA_DIR:-"$root/testdata/upstream"}

mkdir -p "$destination"

fetch() {
    name=$1
    repository=$2
    revision=$3
    target="$destination/$name"

    if [ -d "$target/.git" ]; then
        actual=$(git -C "$target" rev-parse HEAD)
        if [ "$actual" = "$revision" ]; then
            printf '%s already pinned at %s\n' "$name" "$revision"
            return
        fi
        printf '%s exists at %s, expected %s; remove it before refetching\n' "$target" "$actual" "$revision" >&2
        exit 1
    fi
    if [ -e "$target" ]; then
        printf '%s exists but is not a git checkout\n' "$target" >&2
        exit 1
    fi

    printf 'fetching %s at %s\n' "$name" "$revision"
    git init -q "$target"
    git -C "$target" remote add origin "$repository"
    git -C "$target" fetch -q --depth 1 origin "$revision"
    git -C "$target" checkout -q --detach FETCH_HEAD
}

while IFS='|' read -r name repository revision license purpose; do
    case "$name" in
        ''|'#'*) continue ;;
    esac
    fetch "$name" "$repository" "$revision"
done < "$lock"

printf 'corpora are available under %s\n' "$destination"
