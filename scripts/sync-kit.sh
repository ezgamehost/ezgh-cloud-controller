#!/usr/bin/env bash
# Vendors the Go kit into this service, as internal/kit. Private cross-repo Go modules aren't set
# up in CI, so a service built from the template carries a copy of the kit and records where it
# came from (internal/kit/KIT_VERSION); run this again to update it, review the diff, commit.
#
#   scripts/sync-kit.sh                      the latest main of ezgamehost/service-template
#   scripts/sync-kit.sh <ref>                a branch, tag or commit of it
#   scripts/sync-kit.sh --from <checkout>    a local service-template checkout, as it is
#
# It copies kit/go (without its go.mod), the contracts' test vectors and examples the kit's tests
# read (internal/kit/testdata/contracts), and rewrites the kit's import path to this module's.
set -euo pipefail

cd "$(dirname "$0")/.."

KIT_MODULE=github.com/ezgamehost/service-template/kit/go
REPO_URL=${SERVICE_TEMPLATE_REPO:-https://github.com/ezgamehost/service-template.git}

if grep -q "^replace $KIT_MODULE" go.mod 2>/dev/null; then
  echo "error: this is the template inside service-template, which builds against ../../kit/go; there's nothing to vendor" >&2
  exit 1
fi

src=""
ref=main
case ${1:-} in
  --from) src=$(cd "${2:?usage: sync-kit.sh --from <service-template checkout>}" && pwd) ;;
  "") ;;
  *) ref=$1 ;;
esac

tmp=$(mktemp -d)
synced=false
# On any failure after the backup below, go.mod, go.sum and internal/kit are put back as they were.
restore() {
  if ! $synced && [[ -d $tmp/backup ]]; then
    cp "$tmp/backup/go.mod" go.mod
    if [[ -f $tmp/backup/go.sum ]]; then cp "$tmp/backup/go.sum" go.sum; else rm -f go.sum; fi
    rm -rf internal/kit
    if [[ -d $tmp/backup/kit ]]; then mv "$tmp/backup/kit" internal/kit; fi
    echo "sync-kit: failed; go.mod, go.sum and internal/kit are as they were" >&2
  fi
  rm -rf "$tmp"
}
trap restore EXIT
if [[ -z $src ]]; then
  git clone --quiet --filter=blob:none "$REPO_URL" "$tmp/service-template"
  git -C "$tmp/service-template" checkout --quiet "$ref"
  src=$tmp/service-template
fi
[[ -f $src/kit/go/go.mod ]] || { echo "error: $src has no kit/go" >&2; exit 1; }

commit=$(git -C "$src" rev-parse HEAD 2>/dev/null || echo unknown)
dirty=""
if [[ -n $(git -C "$src" status --porcelain -- kit/go contracts 2>/dev/null) ]]; then
  dirty=" (with uncommitted changes)"
fi
module=$(awk '/^module /{print $2; exit}' go.mod)

kit_version() {
  awk -v m="$1" '$1 == m {print $2; exit} $1 == "require" && $2 == m {print $3; exit}' "$src/kit/go/go.mod"
}
ent_version=$(kit_version entgo.io/ent)
pgx_version=$(kit_version github.com/jackc/pgx/v5)
[[ -n $ent_version && -n $pgx_version ]] || { echo "error: $src/kit/go/go.mod doesn't require entgo.io/ent and github.com/jackc/pgx/v5" >&2; exit 1; }

# The kit's generated ent code must compile against exactly the ent (and Atlas) it was generated
# with, and run on the pgx it was tested with: those are pinned exactly. Every other module ent,
# pgx and the ent generator use is raised to at least the kit's version, never lowered: a service
# may well be on a newer golang.org/x/text or google/uuid than the kit, and keeps it.
exact=" entgo.io/ent ariga.io/atlas github.com/jackc/pgx/v5 "

# vercmp a b: -1, 0 or 1, by semver precedence (Go's pseudo-versions are prereleases).
vercmp() {
  perl -e '
    sub parse { my $v = shift; $v =~ s/^v//; $v =~ s/\+.*//; my ($core, $pre) = split /-/, $v, 2;
      return ([split /\./, $core], defined $pre ? [split /\./, $pre] : undef) }
    my ($ac, $ap) = parse($ARGV[0]); my ($bc, $bp) = parse($ARGV[1]);
    for my $i (0 .. 2) { my $c = ($ac->[$i] // 0) <=> ($bc->[$i] // 0); if ($c) { print $c; exit } }
    if (!defined $ap || !defined $bp) { print((defined $bp ? 1 : 0) - (defined $ap ? 1 : 0)); exit }
    for my $i (0 .. ($#$ap > $#$bp ? $#$ap : $#$bp)) {
      my ($x, $y) = ($ap->[$i], $bp->[$i]);
      if (!defined $x) { print -1; exit } if (!defined $y) { print 1; exit }
      my $xn = $x =~ /^\d+$/ ? 1 : 0; my $yn = $y =~ /^\d+$/ ? 1 : 0;
      my $c = $xn && $yn ? $x <=> $y : $xn ? -1 : $yn ? 1 : $x cmp $y;
      if ($c) { print $c; exit }
    }
    print 0' "$1" "$2"
}

# version_in list module: module's version in a "path version" list, or nothing.
version_in() { awk -v m="$2" '$1 == m {print $2; exit}' <<<"$1"; }

gets=()
pins=""
if command -v go >/dev/null; then
  # ent, pgx and every module they (and the ent generator) depend on, at the kit's versions: what
  # the kit's go.sum resolved.
  pins=$(cd "$src/kit/go" &&
    go list -deps -test -f '{{if and .Module (or (eq .Module.Path "entgo.io/ent") (eq .Module.Path "github.com/jackc/pgx/v5"))}}{{.ImportPath}}{{end}}' ./... |
    grep -v /internal/ | LC_ALL=C sort -u |
    xargs go list -deps -f '{{with .Module}}{{.Path}}@{{.Version}}{{end}}' entgo.io/ent/cmd/ent | LC_ALL=C sort -u)
  # Decided before anything changes: what this service has now.
  have=$(go list -m -f '{{if not .Main}}{{.Path}} {{.Version}}{{end}}' all)
  for pin in $pins; do
    mod=${pin%@*} want=${pin#*@}
    cur=$(version_in "$have" "$mod")
    c=-1
    [[ -n $cur ]] && c=$(vercmp "$cur" "$want")
    if [[ $exact == *" $mod "* && $c == 1 ]]; then
      echo "error: this service requires $mod $cur, newer than the kit's $want, and the vendored kit needs exactly $want; move the kit to $cur in service-template first" >&2
      exit 1
    fi
    if [[ $c == -1 ]]; then
      gets+=("$pin")
    fi
  done
fi

mkdir -p "$tmp/backup"
cp go.mod "$tmp/backup/"
if [[ -f go.sum ]]; then cp go.sum "$tmp/backup/"; fi
if [[ -d internal/kit ]]; then mv internal/kit "$tmp/backup/kit"; fi
mkdir -p internal/kit
(cd "$src/kit/go" && tar cf - --exclude go.mod --exclude go.sum --exclude scripts --exclude README.md .) | (cd internal/kit && tar xf -)
mkdir -p internal/kit/testdata/contracts
cp -R "$src/contracts/vectors" "$src/contracts/examples" internal/kit/testdata/contracts/

# The kit's import path becomes this module's internal/kit, in the kit and in the service.
find . -name '*.go' -not -path './.git/*' -print0 |
  xargs -0 perl -pi -e "s#\\Q$KIT_MODULE\\E#$module/internal/kit#g"

cat > internal/kit/KIT_VERSION <<EOM
# Vendored by scripts/sync-kit.sh; don't edit the kit here. Change it in service-template and sync.
source: $REPO_URL
path: kit/go
commit: $commit$dirty
synced: $(date -u +%Y-%m-%dT%H:%M:%SZ)
ent: $ent_version
pgx: $pgx_version
EOM

if command -v go >/dev/null; then
  if ((${#gets[@]} > 0)); then
    go get "${gets[@]}"
  fi
  # The generator, as a tool, so the service can generate its own ent code (go tool ent).
  go get -tool "entgo.io/ent/cmd/ent@$ent_version"
  go mod tidy
  built=$(go list -m -f '{{if not .Main}}{{.Path}} {{.Version}}{{end}}' all)
  for pin in $pins; do
    mod=${pin%@*} want=${pin#*@}
    got=$(version_in "$built" "$mod")
    [[ -n $got ]] || continue # not in this service's build
    c=$(vercmp "$got" "$want")
    if [[ $exact == *" $mod "* && $c != 0 ]]; then
      echo "error: $mod resolves to $got here but $want in the kit; the vendored kit needs exactly $want" >&2
      exit 1
    fi
    if [[ $c == -1 ]]; then
      echo "error: $mod resolves to $got here, older than the kit's $want" >&2
      exit 1
    fi
  done
  gofmt -w internal/kit
fi
synced=true
echo "kit $commit$dirty vendored into internal/kit (ent $ent_version, pgx $pgx_version)"
