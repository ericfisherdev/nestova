#!/usr/bin/env bash
# Keeps .sonarcloud.properties in step with the committed Go files it lists
# (NES-208, NES-209).
#
# SonarCloud's automatic analysis reads only that file and does not allow
# wildcard patterns, so paths are listed by hand: every committed *_templ.go in
# sonar.exclusions, and those plus every committed *_test.go in
# sonar.cpd.exclusions (tests stay analysed, only duplication is ignored). This
# check fails when a file is missing from a list, or when a listed path no
# longer exists, and prints the corrected lines.
#
# usage: scripts/check-sonarcloud-exclusions.sh [--write]
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

props=.sonarcloud.properties
templ_files=$(git ls-files '*_templ.go' | LC_ALL=C sort | paste -sd, -)
cpd_files=$(git ls-files '*_templ.go' '*_test.go' | LC_ALL=C sort -u | paste -sd, -)

value_of() { sed -n "s/^$1=//p" "$props"; }

status=0
check_key() {
  local key=$1 want=$2
  if [ "$(value_of "$key")" != "$want" ]; then
    echo "$props: $key is out of date with the committed Go files" >&2
    status=1
  fi
}
check_key sonar.exclusions "$templ_files"
check_key sonar.cpd.exclusions "$cpd_files"

if [ "$status" -ne 0 ]; then
  if [ "${1:-}" = "--write" ]; then
    sed -i -e "s|^sonar.exclusions=.*|sonar.exclusions=$templ_files|" \
           -e "s|^sonar.cpd.exclusions=.*|sonar.cpd.exclusions=$cpd_files|" "$props"
    echo "$props rewritten" >&2
    exit 0
  fi
  echo "run: scripts/check-sonarcloud-exclusions.sh --write" >&2
  exit 1
fi
