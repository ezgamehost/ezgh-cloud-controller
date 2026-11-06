#!/usr/bin/env bash
# Copies ezgh-cli's API clients (Go internal/ packages, so they can't be imported from here) and the
# OpenAPI documents their contract tests check them against, at a named commit, and records it in
# SOURCE. The copies are unedited apart from import paths: change them in ezgh-cli and sync again.
# What this repository adds lives in internal/ezghclient/*.go, or beside a copy in files named
# kubernetes*.go (methods the CLI lacks), which a sync keeps. Run it in ezgh-cloud-controller only:
# the other repositories copy internal/ezghclient from there (scripts/sync-copies.sh).
#
#   internal/ezghclient/sync.sh [<ezgh-cli commit>]   (default: origin/main of ../ezgh-cli)
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
grep -qx 'module github.com/ezgamehost/ezgh-cloud-controller' "$here/../../go.mod" ||
  { echo "sync.sh: run it in ezgh-cloud-controller; this repository copies internal/ezghclient from there (scripts/sync-copies.sh)" >&2; exit 2; }
cli="${EZGH_CLI:-$here/../../../ezgh-cli}"
[[ -d $cli/.git || -f $cli/.git ]] || cli="$HOME/code/ezgh-cli"
[[ -n ${1:-} ]] || git -C "$cli" fetch -q origin
rev="$(git -C "$cli" rev-parse "${1:-origin/main}")"

packages=(apiclient ids openapitest lbapi blockstoreapi computeapi networkapi filesapi secretsapi certificatesapi)
documents=(lb-api blockstore-api compute-api network-api files-api certificates-api)

for p in "${packages[@]}"; do
  find "$here/$p" -maxdepth 1 -name '*.go' ! -name 'kubernetes*.go' -delete 2>/dev/null || true
  mkdir -p "$here/$p"
  for f in $(git -C "$cli" ls-tree --name-only "$rev" "internal/$p/"); do
    git -C "$cli" show "$rev:$f" |
      sed -e 's#github.com/ezgamehost/ezgh-cli/internal/#github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/#g' \
          -e 's#"\.\./\.\./api/openapi/#"../openapi/#g' >"$here/$p/$(basename "$f")"
  done
done
mkdir -p "$here/openapi"
for d in "${documents[@]}"; do
  git -C "$cli" show "$rev:api/openapi/$d.json" >"$here/openapi/$d.json"
done
echo "$rev" >"$here/SOURCE"
gofmt -l "$here" | (! grep .) || { echo "sync.sh: copies aren't gofmt-ed" >&2; exit 1; }
echo "synced ezgh-cli $rev"
