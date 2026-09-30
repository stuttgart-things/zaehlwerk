#!/usr/bin/env bash
# The chain piezo → zaehlwerk → Schmetterpause, played once through the real
# binaries: the same thing `task chain:schmetterpause`, `task chain:run` and
# `task chain:piezo` do by hand, without the hands. CI runs it on every pull
# request (chain-e2e.yaml); `task chain:e2e` runs it locally.
#
# What it proves, and what the unit tests cannot:
#   1. a best of three started on the page reaches Schmetterpause as a result
#   2. an event sent twice is a duplicate and does not move the score — the
#      board stops with an error if it does, and zaehlwerk logs the duplicate
#   3. a result refused by Schmetterpause gets there on the page's retry
#
# Ports are off the usual ones, so it runs next to a `task run` without
# either noticing.
set -euo pipefail

ZW_PORT=${E2E_ZW_PORT:-18080}
SP_PORT=${E2E_SP_PORT:-18082}
TOKEN=chain-e2e
ZW=http://localhost:$ZW_PORT
SP=http://localhost:$SP_PORT

WORK=${E2E_WORK:-$(mktemp -d)}
mkdir -p "$WORK"

finish() {
  status=$?
  kill $(jobs -p) 2>/dev/null || true
  if [ $status -ne 0 ]; then
    for log in sp zw piezo; do
      echo "── $log.log (last 40 lines) ──" >&2
      tail -n 40 "$WORK/$log.log" >&2 || true
    done
  fi
  echo "logs in $WORK"
}
trap finish EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# wait_until <seconds> <command…> — polls rather than sleeps a fixed time,
# because the reporter posts from a goroutine and a slow runner is not a
# broken chain.
wait_until() {
  local deadline=$(( SECONDS + $1 )); shift
  until "$@" >/dev/null 2>&1; do
    [ $SECONDS -lt $deadline ] || return 1
    sleep 0.2
  done
}

results() { curl -sf "$SP/results" | jq length; }
has_results() { [ "$(results)" = "$1" ]; }
mode() { curl -sf -X POST "$SP/mode" -d "mode=$1" >/dev/null; }

echo "building"
go build -o "$WORK/zaehlwerk-api" ./cmd/zaehlwerk-api
go build -o "$WORK/chain-mock" ./tools/chain-mock

SCHMETTERPAUSE_ADDR=":$SP_PORT" SCHMETTERPAUSE_TOKEN=$TOKEN \
  "$WORK/chain-mock" schmetterpause >"$WORK/sp.log" 2>&1 &
HTTP_ADDR=":$ZW_PORT" SCHMETTERPAUSE_URL=$SP SCHMETTERPAUSE_TOKEN=$TOKEN \
  "$WORK/zaehlwerk-api" >"$WORK/zw.log" 2>&1 &

wait_until 20 curl -sf "$ZW/healthz" || fail "zaehlwerk did not come up"
wait_until 20 curl -sf "$SP/mode" || fail "the fake Schmetterpause did not come up"

# A resend share high enough that a best of three is certain to send some.
board() {
  ZAEHLWERK_URL=$ZW SCHMETTERPAUSE_URL=$SP SCHMETTERPAUSE_TOKEN=$TOKEN \
    PIEZO_PACE=0s PIEZO_BEST_OF=3 PIEZO_RESEND=0.3 PIEZO_SEED=$1 \
    "$WORK/chain-mock" piezo >>"$WORK/piezo.log" 2>&1
}

echo "1. a best of three, from the page to Schmetterpause"
board 1 || fail "the board stopped with an error"
wait_until 10 has_results 1 || fail "the result did not arrive (have $(results))"

echo "2. an event sent twice is a duplicate"
resent=$(grep -c 'msg=resent' "$WORK/piezo.log" || true)
duplicates=$(grep -c '"outcome":"duplicate"' "$WORK/zw.log" || true)
[ "$resent" -gt 0 ] || fail "the board resent nothing, so nothing was tested"
[ "$duplicates" -ge "$resent" ] ||
  fail "$resent events resent, zaehlwerk saw $duplicates duplicates"

echo "3. a refused result gets through on the retry"
mode refuse
board 2 || fail "the board stopped with an error"
wait_until 10 grep -q 'the result did not reach schmetterpause' "$WORK/zw.log" ||
  fail "zaehlwerk did not report the refused result"
has_results 1 || fail "a refused result was stored anyway"

match=$(grep 'msg=joined' "$WORK/piezo.log" | tail -n 1 | grep -o 'match_id=[0-9a-f]*' | cut -d= -f2)
[ -n "$match" ] || fail "no match id in the board's log"
mode accept
curl -sf -X POST "$ZW/ui/matches/$match/report" >/dev/null || fail "the retry was not accepted"
wait_until 10 has_results 2 || fail "the retried result did not arrive (have $(results))"

echo "ok: 2 results, $resent resends, $duplicates duplicates, retry after refuse"
