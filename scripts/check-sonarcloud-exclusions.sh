#!/usr/bin/env bash
# Keeps .sonarcloud.properties in step with the generated templ files (NES-208).
#
# SonarCloud's automatic analysis reads only that file and does not allow
# wildcard patterns, so every committed *_templ.go path is listed by hand in
# sonar.exclusions and sonar.cpd.exclusions. This check fails when a generated
# file is missing from either list, or when a listed path no longer exists, and
# prints the corrected lines.
#
# usage: scripts/check-sonarcloud-exclusions.sh [--write]
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

props=.sonarcloud.properties
want=$(git ls-files '*_templ.go' | LC_ALL=C sort | paste -sd, -)

value_of() { sed -n "s/^$1=//p" "$props"; }

status=0
for key in sonar.exclusions sonar.cpd.exclusions; do
  have=$(value_of "$key")
  if [ "$have" != "$want" ]; then
    echo "$props: $key is out of date with the generated templ files" >&2
    status=1
  fi
done

if [ "$status" -ne 0 ]; then
  if [ "${1:-}" = "--write" ]; then
    sed -i -e "s|^sonar.exclusions=.*|sonar.exclusions=$want|" \
           -e "s|^sonar.cpd.exclusions=.*|sonar.cpd.exclusions=$want|" "$props"
    echo "$props rewritten" >&2
    exit 0
  fi
  echo "run: scripts/check-sonarcloud-exclusions.sh --write" >&2
  exit 1
fi
