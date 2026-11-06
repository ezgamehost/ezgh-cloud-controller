#!/usr/bin/env bash
# Refreshes the code this repository copies from other ezgamehost repositories (COPIES lists each
# path and the commit it was copied at). Copies are unedited apart from the Go module path: change
# them at their source and sync again; review the diff and commit.
#
#   scripts/sync-copies.sh <repository> [<commit>]   (default: origin/main of ../<repository>)
set -euo pipefail
cd "$(dirname "$0")/.."
repo=${1:?usage: scripts/sync-copies.sh <repository> [<commit>]}
src=${SYNC_FROM:-../$repo}
[[ -e $src/.git ]] || src=$HOME/code/$repo
[[ -n ${2:-} ]] || git -C "$src" fetch -q origin
rev=$(git -C "$src" rev-parse "${2:-origin/main}")
from=$(git -C "$src" show "$rev:go.mod" | awk '/^module /{print $2; exit}')
to=$(awk '/^module /{print $2; exit}' go.mod)
paths=$(awk -v r="$repo" '$1 == r {print $3}' COPIES)
[[ -n $paths ]] || { echo "sync-copies: COPIES lists nothing from $repo" >&2; exit 2; }
for p in $paths; do
  rm -rf "$p"
  files=$(git -C "$src" ls-tree -r "$rev" -- "$p")
  [[ -n $files ]] || { echo "sync-copies: $repo@$rev has no $p" >&2; exit 1; }
  while read -r mode _ _ f; do
    mkdir -p "$(dirname "$f")"
    git -C "$src" show "$rev:$f" | perl -pe "s#\\Q$from\\E(?=/)#$to#g" >"$f"
    [[ $mode != 100755 ]] || chmod +x "$f"
  done <<<"$files"
done
awk -v r="$repo" -v c="$rev" '$1 == r {$2 = c} {print}' COPIES >COPIES.tmp && mv COPIES.tmp COPIES
echo "synced $repo $rev"
