#!/usr/bin/env bash
# Service code logs through the kit (contracts/deployment.md, "Logging and shutdown"): it never
# builds a slog handler of its own. Kit code, tests, fakes, spikes, benches, tools and
# discard-only loggers are exempt. CI runs it in every service that vendors the kit: services
# carry a copy as scripts/check-logging.sh, as they do check-data-layer.sh (sync-kit doesn't copy
# the kit's scripts); the template's copy is this file (CI compares them).
#
#   scripts/check-logging.sh   (kit/go/scripts/check-logging.sh in service-template)
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# glob magic, so **/ also matches the repository's root (internal/kit/ in a service).
x=':(exclude,glob)'
hits=$(git grep -nE 'slog\.New(JSON|Text)Handler\(' -- '*.go' \
  "${x}kit/go/**" "${x}**/internal/kit/**" "${x}**/*_test.go" "${x}**/testdata/**" "${x}**/*test/**" \
  "${x}**/fake*/**" "${x}**/cmd/fake-*/**" "${x}**/spikes/**" "${x}**/bench/**" "${x}**/tools/**" \
  "${x}**/conformance/**" "${x}**/*loadtest*/**" | grep -v 'io\.Discard' || true)
if [[ -n $hits ]]; then
  echo "check-logging: build loggers with the kit's logging.New, not slog handlers of your own:" >&2
  echo "$hits" >&2
  exit 1
fi
echo "check-logging: ok"
