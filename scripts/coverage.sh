#!/usr/bin/env bash
# Measure statement coverage of dbferry's OWN packages across every suite (unit
# + integration + fault-injection) and enforce a minimum threshold (poc-plan
# 0.4). Needs the stand running (`make stand-up`).
#
# Threshold exclusions: coverage is attributed only to the packages listed in
# COVER_PKGS via -coverpkg, so the threshold ignores CLI glue (cmd/dbferry), the
# test harness (test/...), and dev tools (test/integration/agekeygen). Add the
# driver package to COVER_PKGS when it lands (poc-plan 4.1).
set -euo pipefail

THRESHOLD="${COVER_THRESHOLD:-85}"
COVER_PKGS="github.com/dbferry/dbferry/pipeline,github.com/dbferry/dbferry/config"

# Profile path is kept (gitignored *.out) so CI can archive it as an artifact.
profile="${COVER_PROFILE:-cover.out}"

go test -tags=integration,faultinjection \
  -coverpkg="$COVER_PKGS" -coverprofile="$profile" ./... >/dev/null

go tool cover -func="$profile" | tail -1
total="$(go tool cover -func="$profile" | awk '/^total:/ {print substr($3, 1, length($3)-1)}')"

awk -v t="$total" -v thr="$THRESHOLD" 'BEGIN {
  if (t + 0 < thr + 0) { printf "FAIL: %s coverage %.1f%% < %d%% threshold\n", "'"$COVER_PKGS"'", t, thr; exit 1 }
  printf "OK: coverage %.1f%% >= %d%% threshold\n", t, thr
}'
