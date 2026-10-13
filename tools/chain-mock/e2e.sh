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
#   4. zaehlwerk-pong plays headless best of threes, one of them to a deciding
#      set with its change of ends at five, and its truth and zaehlwerk
#      agree after every rally (ADR-0006)
#   5. a Pong match never reaches Schmetterpause
#
# Ports are off the usual ones, so it runs next to a `task run` without
# either noticing.
set -euo pipefail

ZW_PORT=${E2E_ZW_PORT:-18080}
SP_PORT=${E2E_SP_PORT:-18082}
PONG_PORT=${E2E_PONG_PORT:-18081}
TOKEN=chain-e2e
ZW=http://localhost:$ZW_PORT
SP=http://localhost:$SP_PORT
PONG=http://localhost:$PONG_PORT

WORK=${E2E_WORK:-$(mktemp -d)}
mkdir -p "$WORK"

finish() {
  status=$?
  kill $(jobs -p) 2>/dev/null || true
  if [ $status -ne 0 ]; then
    for log in sp zw piezo pong; do
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
go build -o "$WORK/zaehlwerk-pong" ./cmd/zaehlwerk-pong

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

# The game, against the same zaehlwerk. It reports nowhere and refuses to
# start while a match is current, so it runs after the board's matches are
# over, and the fake Schmetterpause must still hold exactly their two results.
HTTP_ADDR=":$PONG_PORT" ZAEHLWERK_URL=$ZW PONG_BEST_OF=3 PONG_SEED=1 \
  "$WORK/zaehlwerk-pong" >"$WORK/pong.log" 2>&1 &
wait_until 20 curl -sf "$PONG/healthz" || fail "zaehlwerk-pong did not come up"

game_over() { [ "$(curl -sf "$PONG/games/$1" | jq -r .status)" != playing ]; }

echo "4. headless best of threes agree with zaehlwerk at every rally"
games=0
decider=""
# Each game draws different rallies from the seed, so some go to a third
# set and some do not. The seed makes it the same games on every run; ten is
# far more than it takes.
while [ $games -lt 10 ] && [ -z "$decider" ]; do
  id=$(curl -sf -X POST "$PONG/games" -H 'content-type: application/json' \
    -d '{"pace":"0s"}' | jq -r .id) || fail "zaehlwerk-pong did not start a game"
  wait_until 60 game_over "$id" || fail "game $id did not finish"
  game=$(curl -sf "$PONG/games/$id")
  games=$((games + 1))

  [ "$(jq -r .status <<<"$game")" = won ] ||
    fail "game $id: $(jq -c '{status, reason}' <<<"$game")"
  [ "$(jq '.divergences | length' <<<"$game")" = 0 ] ||
    fail "game $id: truth and zaehlwerk part at $(jq -c '.divergences[0]' <<<"$game")"
  [ "$(jq -c .truth <<<"$game")" = "$(jq -c .zaehlwerk <<<"$game")" ] ||
    fail "game $id: truth $(jq -c .truth <<<"$game"), zaehlwerk $(jq -c .zaehlwerk <<<"$game")"
  echo "   game $id: $(jq -c .state.completed_sets <<<"$game"), $(jq .rallies <<<"$game") rallies, no divergence"

  [ "$(jq .state.set_number <<<"$game")" = 3 ] && decider=$game
done
[ -n "$decider" ] || fail "$games best of threes and none went to a deciding set"
jq -e '[.ends_changes[] | select(.set == 3 and (.points | max) == 5)] | length == 1' \
  <<<"$decider" >/dev/null ||
  fail "no change of ends at five in the deciding set: $(jq -c .ends_changes <<<"$decider")"

echo "5. a Pong match never reaches Schmetterpause"
# The one fixed wait in here: there is nothing to poll for when the check is
# that something does not arrive. The reporter posts within milliseconds of
# match_won, so a second is ample for a result that should not exist.
sleep 1
has_results 2 || fail "a Pong match was reported (have $(results))"

echo "ok: 2 results, $resent resends, $duplicates duplicates, retry after refuse," \
  "$games Pong games with no divergence, ends changed at five in the decider"
