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
#   4. a match point given to the wrong player and undone in the grace period
#      is never reported, and the right result is, once (ADR-0007)
#   5. zaehlwerk-pong plays headless best of threes, one of them to a deciding
#      set with its change of ends at five, and its truth and zaehlwerk
#      agree after every rally (ADR-0006)
#   6. a game with every fault injected ends with no divergence the faults do
#      not explain: resends discarded, a second confirmation ignored, a
#      counter reset after a quiet minute accepted, a wrong half undone
#   7. a game played the way the browser plays it — presses with a token,
#      one number each — ends with truth and zaehlwerk agreeing; a press
#      without the token is refused and one sent twice counts once
#   8. a game on two devices, a curl host and a curl guest: the guest joins
#      with the code, its token presses nothing, its ready goes over the
#      relay and reaches zaehlwerk only as the host's press, and truth and
#      zaehlwerk agree at the end (ADR-0009)
#   9. attract mode starts a demo on a free table after the wait, with no
#      faults whatever PONG_FAULTS says; a match started on the page ends it
#      inside zaehlwerk at once and the buttons count into that match, a
#      demo asked for while it runs is refused and Schmetterpause hears of
#      neither, and the next demo waits for that match to end; a game asked
#      for takes the table from a demo (ADR-0008)
#   10. a button check pressed as it says has no finding, and one press at the
#      wrong half is the one finding it reports
#   11. a Pong match never reaches Schmetterpause
#   12. two fobs check in at the two halves and start a match, a retried
#      check-in resolves once, an unknown fob and one during the match change
#      nothing, the match is played, and its result reaches Schmetterpause
#      with both check-ins and no operator; no UID is in either log (ADR-0010)
#
# Ports are off the usual ones, so it runs next to a `task run` without
# either noticing.
set -euo pipefail

ZW_PORT=${E2E_ZW_PORT:-18080}
SP_PORT=${E2E_SP_PORT:-18082}
PONG_PORT=${E2E_PONG_PORT:-18081}
TOKEN=chain-e2e
HUB_TOKEN=chain-e2e-hub
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
# because the reporter posts after its grace period, from a goroutine, and a
# slow runner is not a broken chain.
wait_until() {
  local deadline=$(( SECONDS + $1 )); shift
  until "$@" >/dev/null 2>&1; do
    [ $SECONDS -lt $deadline ] || return 1
    sleep 0.2
  done
}

results() { curl -sf "$SP/results" | jq length; }
# scheduled — how many results zaehlwerk has set out to report. Counted in
# its log, where the reporter says so at match_won, before the grace period.
scheduled() { grep -c '"msg":"report scheduled"' "$WORK/zw.log" || true; }
has_results() { [ "$(results)" = "$1" ]; }
mode() { curl -sf -X POST "$SP/mode" -d "mode=$1" >/dev/null; }

echo "building"
go build -o "$WORK/zaehlwerk-api" ./cmd/zaehlwerk-api
go build -o "$WORK/chain-mock" ./tools/chain-mock
go build -o "$WORK/zaehlwerk-pong" ./cmd/zaehlwerk-pong

SCHMETTERPAUSE_ADDR=":$SP_PORT" SCHMETTERPAUSE_TOKEN=$TOKEN \
  "$WORK/chain-mock" schmetterpause >"$WORK/sp.log" 2>&1 &
# A grace period short enough for CI, and long enough for the undo in step 4,
# which is two curls after the match point.
HTTP_ADDR=":$ZW_PORT" SCHMETTERPAUSE_URL=$SP SCHMETTERPAUSE_TOKEN=$TOKEN SCHMETTERPAUSE_REPORT_DELAY=2s \
  CHECKIN_TOKEN=$HUB_TOKEN \
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

echo "4. a match point undone in the grace period is not reported"
roster=$(curl -sf -H "Authorization: Bearer $TOKEN" "$SP/api/players")
curl -sf -X POST "$ZW/ui/matches" -d best_of=1 \
  -d "home_id=$(jq -r '.[0].id' <<<"$roster")" -d "away_id=$(jq -r '.[1].id' <<<"$roster")" \
  -d "operator_id=$(jq -r '.[2].id' <<<"$roster")" >/dev/null || fail "the page did not start the match"
match=$(curl -sf "$ZW/matches/current" | jq -r .match_id)
event=0
press() {
  event=$((event + 1))
  curl -sf -X POST "$ZW/ingest/web" -H 'Content-Type: application/json' \
    -d "{\"match_id\":\"$match\",\"source\":\"e2e-undo\",\"player\":\"$1\",\"event_id\":$event}" >/dev/null ||
    fail "press $event for $1 was refused"
}
for _ in $(seq 9); do press a; done
for _ in $(seq 10); do press b; done
# 9:10, and the point a should have levelled with goes to b, who wins 9:11.
press b
[ "$(curl -sf "$ZW/matches/$match" | jq -r .complete)" = true ] || fail "the wrong match point did not win the match"
curl -sf -X POST "$ZW/matches/$match/undo" >/dev/null || fail "the undo was refused"
grep -q '"msg":"report withheld, the match was reopened"' "$WORK/zw.log" ||
  fail "zaehlwerk did not withhold the report on the undo"
# The right point, and a wins 12:10.
for _ in $(seq 3); do press a; done
wait_until 10 has_results 3 || fail "the right result did not arrive (have $(results))"
sets=$(curl -sf "$SP/results" | jq -c '.[-1].sets')
[ "$sets" = '[[12,10]]' ] || fail "Schmetterpause got $sets, want [[12,10]]"
[ "$(grep '"msg":"result reported to schmetterpause"' "$WORK/zw.log" | grep -c "\"match_id\":\"$match\"")" = 1 ] ||
  fail "match $match was reported more than once"
reported=$(scheduled)

# The game, against the same zaehlwerk. It reports nowhere and refuses to
# start while a match is current, so it runs after the board's matches are
# over, and the fake Schmetterpause must still hold exactly their three results.
HTTP_ADDR=":$PONG_PORT" ZAEHLWERK_URL=$ZW PONG_BEST_OF=3 PONG_SEED=1 \
  "$WORK/zaehlwerk-pong" >"$WORK/pong.log" 2>&1 &
pong_pid=$!
wait_until 20 curl -sf "$PONG/healthz" || fail "zaehlwerk-pong did not come up"

game_over() { [ "$(curl -sf "$PONG/games/$1" | jq -r .status)" != playing ]; }

echo "5. headless best of threes agree with zaehlwerk at every rally"
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

echo "6. a game with every fault has no unexplained divergence"
# The seed fixes which presses go wrong. counter_reset waits out zaehlwerk's
# quiet minute before each reset, so it is rare here, and this game is the
# one step that takes a while.
faults='{"wrong_half":0.1,"correct":true,"resend":0.1,"bounce":0.05,"lost":0.05,"ghost":0.05,"wrong_phase":0.9,"counter_reset":0.04}'
id=$(curl -sf -X POST "$PONG/games" -H 'content-type: application/json' \
  -d "{\"pace\":\"0s\",\"faults\":$faults}" | jq -r .id) || fail "zaehlwerk-pong did not start a faulted game"
wait_until 400 game_over "$id" || fail "faulted game $id did not finish"
game=$(curl -sf "$PONG/games/$id")
games=$((games + 1))
[ "$(jq -r .status <<<"$game")" = won ] ||
  fail "faulted game $id: $(jq -c '{status, reason}' <<<"$game")"
[ "$(jq .unexplained <<<"$game")" = 0 ] ||
  fail "faulted game $id: a finding at $(jq -c '[.divergences[] | select(.expected | not)][0]' <<<"$game")"
for kind in wrong_half resend bounce lost ghost wrong_phase counter_reset; do
  jq -e --arg k "$kind" '[.faults[] | select(.kind == $k)] | length > 0' <<<"$game" >/dev/null ||
    fail "faulted game $id: no $kind was injected, the seed no longer covers it"
done
jq -e '.zaehlwerk == ([.truth, .explained] | transpose | map(add))' <<<"$game" >/dev/null ||
  fail "faulted game $id: zaehlwerk $(jq -c .zaehlwerk <<<"$game") is not truth $(jq -c .truth <<<"$game") moved by $(jq -c .explained <<<"$game")"
echo "   game $id: $(jq -r '.faults | group_by(.kind) | map("\(length) \(.[0].kind)") | join(", ")' <<<"$game")"
echo "   $(jq '.divergences | length' <<<"$game") divergences, all explained; truth $(jq -c .truth <<<"$game"), zaehlwerk $(jq -c .zaehlwerk <<<"$game")"

echo "7. a game played through the browser's presses"
# What game.js does, without a browser: read the phase from the answer, press
# ready for a half zaehlwerk waits for, report a rally's winning half. The
# canvas itself is checked by hand; this checks the door it reports through.
started=$(curl -sf -X POST "$PONG/games" -H 'content-type: application/json' \
  -d '{"driver":"browser","mode":"pvp","faults":{}}') || fail "zaehlwerk-pong did not start a browser game"
id=$(jq -r .id <<<"$started")
token=$(jq -r .token <<<"$started")
press() {
  curl -s -o "$WORK/press.json" -w '%{http_code}' -X POST "$PONG/games/$id/presses" \
    -H 'content-type: application/json' -H "X-Pong-Token: $1" -d "$2"
}
[ "$(press "" '{"rally":1,"kind":"ready","half":"left"}')" = 403 ] ||
  fail "browser game $id: a press without the token was not refused"
n=1
game=$(curl -sf "$PONG/games/$id")
while [ "$(jq -r .status <<<"$game")" = playing ] && [ $n -lt 400 ]; do
  case $(jq -r .state.phase <<<"$game") in
    warmup | change_ends) body="{\"rally\":$n,\"kind\":\"ready\",\"half\":\"$(jq -r '.state.waiting[0]' <<<"$game")\"}" ;;
    *) body="{\"rally\":$n,\"kind\":\"rally\",\"half\":\"$( ((n % 3)) && echo left || echo right)\"}" ;;
  esac
  [ "$(press "$token" "$body")" = 200 ] || fail "browser game $id: press $n: $(cat "$WORK/press.json")"
  if [ $n = 5 ]; then
    # The same number again, as a page does whose answer got lost.
    before=$(jq -c .zaehlwerk "$WORK/press.json")
    [ "$(press "$token" "$body")" = 200 ] || fail "browser game $id: press $n again was refused"
    [ "$(jq -c .zaehlwerk "$WORK/press.json")" = "$before" ] || fail "browser game $id: press $n counted twice"
  fi
  game=$(cat "$WORK/press.json")
  n=$((n + 1))
done
wait_until 10 game_over "$id" || fail "browser game $id did not finish"
game=$(curl -sf "$PONG/games/$id")
games=$((games + 1))
[ "$(jq -r .status <<<"$game")" = won ] || fail "browser game $id: $(jq -c '{status, reason}' <<<"$game")"
[ "$(jq '.divergences | length' <<<"$game")" = 0 ] ||
  fail "browser game $id: truth and zaehlwerk part at $(jq -c '.divergences[0]' <<<"$game")"
echo "   game $id: $(jq -c .state.completed_sets <<<"$game"), $((n - 1)) presses, no divergence"

echo "8. a game on two devices: the host reports, the guest only moves and says ready"
# What the two pages do, without a browser. The guest's ready goes over the
# relay as a count, and the host presses it; the relay is read the way a
# page reads it, as SSE, in the background.
started=$(curl -sf -X POST "$PONG/games" -H 'content-type: application/json' \
  -d '{"driver":"browser","mode":"remote","faults":{}}') || fail "zaehlwerk-pong did not start a game on two devices"
id=$(jq -r .id <<<"$started")
token=$(jq -r .token <<<"$started")
code=$(jq -r .join_code <<<"$started")
location=$(curl -s -o /dev/null -w '%{redirect_url}' -X POST "$PONG/ui/join" --data-urlencode "code=$code")
guest=${location##*#guest:}
[ -n "$guest" ] && [ "$guest" != "$location" ] || fail "game $id: joining with the code gave $location"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$PONG/ui/join" --data-urlencode "code=$code")" = 409 ] ||
  fail "game $id: the code joined twice"
[ "$(press "$guest" '{"rally":1,"kind":"ready","half":"right"}')" = 403 ] ||
  fail "game $id: the guest's token pressed"
curl -sN "$PONG/games/$id/relay" >"$WORK/relay.txt" &
relay_pid=$!
paddle() {
  curl -s -o /dev/null -w '%{http_code}' -X POST "$PONG/games/$id/paddle" \
    -H 'content-type: application/json' -H "X-Pong-Token: $guest" -d "$1"
}
frame() {
  curl -s -o /dev/null -w '%{http_code}' -X POST "$PONG/games/$id/frames" \
    -H 'content-type: application/json' -H "X-Pong-Token: $token" \
    -d "{\"tick\":$1,\"ball\":{\"x\":480,\"y\":270,\"z\":10},\"resting\":null,\"paddles\":[270,270]}"
}
relayed() { grep -q "\"ready\":$1}" "$WORK/relay.txt"; }
n=1
keys=0
game=$(curl -sf "$PONG/games/$id")
while [ "$(jq -r .status <<<"$game")" = playing ] && [ $n -lt 400 ]; do
  phase=$(jq -r .state.phase <<<"$game")
  bhalf=$(jq -r 'if .state.ends[0] == "b" then "left" else "right" end' <<<"$game")
  [ "$(frame "$n")" = 204 ] || fail "game $id: frame $n refused"
  case $phase in
    warmup | change_ends)
      half=$(jq -r '.state.waiting[0]' <<<"$game")
      if [ "$half" = "$bhalf" ]; then
        # The guest says ready; the host presses it once it sees the count.
        keys=$((keys + 1))
        [ "$(paddle "{\"tick\":$n,\"y\":270,\"ready\":$keys}")" = 204 ] || fail "game $id: paddle refused"
        wait_until 5 relayed "$keys" || fail "game $id: the guest's ready $keys did not reach the host"
        before=$(curl -sf "$PONG/games/$id" | jq -c .state.waiting)
        [ "$before" = "$(jq -c .state.waiting <<<"$game")" ] || fail "game $id: the guest's ready pressed by itself"
      fi
      body="{\"rally\":$n,\"kind\":\"ready\",\"half\":\"$half\"}" ;;
    *)
      [ "$(paddle "{\"tick\":$n,\"y\":$((200 + n % 100)),\"ready\":$keys}")" = 204 ] || fail "game $id: paddle refused"
      body="{\"rally\":$n,\"kind\":\"rally\",\"half\":\"$( ((n % 3)) && echo left || echo right)\"}" ;;
  esac
  [ "$(press "$token" "$body")" = 200 ] || fail "game $id: press $n: $(cat "$WORK/press.json")"
  game=$(cat "$WORK/press.json")
  n=$((n + 1))
done
kill "$relay_pid" 2>/dev/null || true
wait_until 10 game_over "$id" || fail "game $id on two devices did not finish"
game=$(curl -sf "$PONG/games/$id")
games=$((games + 1))
[ "$(jq -r .status <<<"$game")" = won ] || fail "game $id: $(jq -c '{status, reason}' <<<"$game")"
[ "$(jq '.divergences | length' <<<"$game")" = 0 ] ||
  fail "game $id: truth and zaehlwerk part at $(jq -c '.divergences[0]' <<<"$game")"
[ "$(jq -c .truth <<<"$game")" = "$(jq -c .zaehlwerk <<<"$game")" ] || fail "game $id: truth and zaehlwerk differ"
frames=$(grep -c '^event: frame' "$WORK/relay.txt" || true)
samples=$(grep -c '^event: guest' "$WORK/relay.txt" || true)
[ "$frames" -gt 0 ] && [ "$samples" -gt 0 ] || fail "game $id: the relay carried $frames frames and $samples samples"
echo "   game $id: $(jq -c .state.completed_sets <<<"$game"), $((n - 1)) presses by the host, $keys readies from the guest," \
  "$frames frames and $samples samples relayed, truth $(jq -c .truth <<<"$game") = zaehlwerk"

echo "9. attract mode: a demo on a free table, and it always gives way"
# The same binary again, with attract mode on and a wait short enough for CI.
# PONG_FAULTS is set to show it does not reach a demo.
kill "$pong_pid"
wait "$pong_pid" 2>/dev/null || true
HTTP_ADDR=":$PONG_PORT" ZAEHLWERK_URL=$ZW PONG_BEST_OF=3 PONG_SEED=1 PONG_FAULTS=lost=1 \
  PONG_ATTRACT_AFTER=2s PONG_ATTRACT_PACE=20ms \
  "$WORK/zaehlwerk-pong" >"$WORK/pong.log" 2>&1 &
wait_until 20 curl -sf "$PONG/healthz" || fail "zaehlwerk-pong with attract mode did not come up"
up=$SECONDS

# The log is the one place that names a demo's game; the API has no list.
demos() { jq -r 'select(.msg | startswith("the table was free")) | .game' "$WORK/pong.log"; }
demo_count() { demos | wc -l; }
has_demos() { [ "$(demo_count)" -ge "$1" ]; }
current() { curl -sf "$ZW/matches/current" | jq -r .match_id; }

wait_until 20 has_demos 1 || fail "attract mode started no demo on a free table"
[ $((SECONDS - up)) -ge 1 ] || fail "the demo did not wait"
id=$(demos | head -1)
game=$(curl -sf "$PONG/games/$id")
[ "$(jq -r .attract <<<"$game")" = true ] || fail "demo $id is not marked as one"
[ "$(jq -c '[.fault_shares[] | select(. != 0 and . != false)] | length' <<<"$game")" = 0 ] ||
  fail "demo $id runs with faults: $(jq -c .fault_shares <<<"$game")"
wait_until 60 game_over "$id" || fail "demo $id did not finish"
game=$(curl -sf "$PONG/games/$id")
[ "$(jq -r .status <<<"$game")" = won ] || fail "demo $id: $(jq -c '{status, reason}' <<<"$game")"
[ "$(jq '.faults | length' <<<"$game")" = 0 ] || fail "demo $id injected faults"
[ "$(jq '.divergences | length' <<<"$game")" = 0 ] ||
  fail "demo $id: truth and zaehlwerk part at $(jq -c '.divergences[0]' <<<"$game")"
games=$((games + 1))
echo "   demo $id: $(jq -c .state.completed_sets <<<"$game"), no faults, no divergence"

# The next demo, after a whole wait. A match started on the page ends it,
# inside zaehlwerk and at once (ADR-0008): its match takes no press any more
# before the game has even seen the table change.
wait_until 20 has_demos 2 || fail "no second demo after the first was won"
id=$(demos | sed -n 2p)
wait_until 10 sh -c "curl -sf '$PONG/games/$id' | jq -e '.match_id != \"\"'" || fail "demo $id has no match"
demo_match=$(curl -sf "$PONG/games/$id" | jq -r .match_id)
[ "$(curl -sf "$ZW/matches/$demo_match" | jq -r .complete)" = false ] || fail "demo $id was over before the page started"
curl -sf -X POST "$ZW/ui/matches" -d best_of=3 \
  -d "home_id=$(jq -r '.[0].id' <<<"$roster")" -d "away_id=$(jq -r '.[1].id' <<<"$roster")" \
  -d "operator_id=$(jq -r '.[2].id' <<<"$roster")" >/dev/null || fail "the page did not start a match"
real=$(current)
[ "$real" != "$demo_match" ] || fail "the page's match is not current"
late=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$ZW/ingest/web" -H 'content-type: application/json' \
  -d "{\"match_id\":\"$demo_match\",\"source\":\"e2e-late\",\"player\":\"a\",\"event_id\":1}")
[ "$late" = 409 ] || fail "demo match $demo_match still takes presses after the page started $real ($late)"
demo_points=$(curl -sf "$ZW/matches/$demo_match" | jq -c .points)
wait_until 10 game_over "$id" || fail "demo $id did not give way to match $real"
[ "$(curl -sf "$PONG/games/$id" | jq -r .reason)" = "another match became current at the table" ] ||
  fail "demo $id ended for another reason: $(curl -sf "$PONG/games/$id" | jq -c '{status, reason}')"
# The buttons count into the page's match, which they resolve to.
for n in 1 2 3; do
  curl -sf -X POST "$ZW/ingest/button" -H 'content-type: application/json' \
    -d "{\"events\":[{\"source\":\"e2e-demo-button\",\"half\":\"left\",\"event_id\":$n}]}" >/dev/null ||
    fail "button press $n was not taken"
done
[ "$(curl -sf "$ZW/matches/$real" | jq '.points | add')" = 3 ] ||
  fail "the buttons did not count into match $real: $(curl -sf "$ZW/matches/$real" | jq -c .points)"
[ "$(curl -sf "$ZW/matches/$demo_match" | jq -c .points)" = "$demo_points" ] ||
  fail "a button press reached demo match $demo_match"
# What attract mode sends, had it seen a free table an instant before the page
# started: refused, with the match that has the table.
refused=$(curl -s -w '\n%{http_code}' -X POST "$ZW/matches" -H 'content-type: application/json' \
  -d '{"players":["Pong A","Pong B"],"best_of":3,"demo":true}')
[ "$(tail -n 1 <<<"$refused")" = 409 ] || fail "a demo was created next to match $real: $refused"
[ "$(head -n 1 <<<"$refused" | jq -r .state.match_id)" = "$real" ] || fail "the refusal does not name $real: $refused"
# Nothing starts while that match is current, for longer than the wait.
sleep 5
[ "$(demo_count)" = 2 ] || fail "a demo started while match $real was current"
[ "$(current)" = "$real" ] || fail "match $real was ended by zaehlwerk-pong"
curl -sf -X POST "$ZW/matches/$real/end" >/dev/null || fail "could not end match $real"
[ "$(scheduled)" = "$reported" ] && has_results 3 || fail "Schmetterpause heard of the demo or the unfinished match"
echo "   demo $id ended at once by match $real from the page, the buttons counted there," \
  "a demo asked for next to it refused, and the next waited for it to end"

# The next one, after that match; a game asked for takes the table from it.
wait_until 20 has_demos 3 || fail "no demo after match $real ended"
id=$(demos | sed -n 3p)
started=$(curl -sf -X POST "$PONG/games" -H 'content-type: application/json' \
  -d '{"driver":"browser","mode":"pvp"}') || fail "a browser game did not get the table from demo $id"
game_id=$(jq -r .id <<<"$started")
[ "$(curl -sf "$PONG/games/$id" | jq -r .reason)" = "gave the table to a new game" ] ||
  fail "demo $id: $(curl -sf "$PONG/games/$id" | jq -c '{status, reason}')"
curl -sf -X POST "$PONG/games/$game_id/end" -H "X-Pong-Token: $(jq -r .token <<<"$started")" >/dev/null ||
  fail "could not end browser game $game_id"
echo "   demo $id gave way to browser game $game_id"

echo "10. a button check, pressed the way the buttons press"
# The buttons are a curl each: /ingest/button with a half, a source and a
# counter per button, as the hardware sends. The game sends nothing; this is
# what arrives at zaehlwerk. A demo may hold the table by now, and the check
# takes it.
declare -A pressed=([left]=0 [right]=0)
button() {
  pressed[$1]=$((pressed[$1] + 1))
  curl -sf -X POST "$ZW/ingest/button" -H 'content-type: application/json' \
    -d "{\"events\":[{\"source\":\"e2e-button-$1\",\"half\":\"$1\",\"event_id\":${pressed[$1]}}]}" \
    >/dev/null || fail "button $1 was not taken"
}
# button_check <wrong> — follows the prompts, pressing the other half at the
# first prompt in play if wrong is 1, and leaves the finished check in $game.
button_check() {
  local wrong=$1 last=0 n half
  id=$(curl -sf -X POST "$PONG/games" -H 'content-type: application/json' -d '{"driver":"check"}' | jq -r .id) ||
    fail "zaehlwerk-pong did not start a button check"
  while game=$(curl -sf "$PONG/games/$id") && [ "$(jq -r .status <<<"$game")" = playing ]; do
    n=$(jq '.check.prompt.n // 0' <<<"$game")
    if [ "$n" -gt "$last" ]; then
      half=$(jq -r .check.prompt.half <<<"$game")
      if [ "$wrong" = 1 ] && [ "$(jq -r .check.prompt.phase <<<"$game")" = playing ]; then
        wrong=0
        [ "$half" = left ] && half=right || half=left
      fi
      button "$half"
      last=$n
    fi
    [ "$last" -lt 80 ] || fail "button check $id does not end"
    sleep 0.05
  done
}

button_check 0
[ "$(jq -r .status <<<"$game")" = checked ] || fail "button check $id: $(jq -c '{status, reason}' <<<"$game")"
[ "$(jq '.check.findings | length' <<<"$game")" = 0 ] ||
  fail "button check $id: $(jq -r .check.summary <<<"$game")"
[ "$(jq '.check.counts.ok == .check.prompts' <<<"$game")" = true ] ||
  fail "button check $id: $(jq -c .check.counts <<<"$game")"
echo "   check $id: $(jq .check.prompts <<<"$game") prompts, all pressed right"

button_check 1
[ "$(jq -r .status <<<"$game")" = checked ] || fail "button check $id: $(jq -c '{status, reason}' <<<"$game")"
[ "$(jq -c '[.check.findings[].outcome]' <<<"$game")" = '["wrong_half"]' ] ||
  fail "button check $id: want one wrong half, got $(jq -c .check.counts <<<"$game")"
jq -r .check.summary <<<"$game" | grep -q 'wrong half 1' || fail "button check $id: the summary does not say it"
echo "   check $id: the wrong half found, $(jq -r '.check.findings[0] | "\(.want) -> \(.got)"' <<<"$game")"

echo "11. a Pong match never reaches Schmetterpause"
# Not a wait for something that should not arrive: the reporter decides at
# match_won whether a match is reported and logs it then, before any grace
# period, so a Pong match that was going to be reported would be counted
# here already.
[ "$(scheduled)" = "$reported" ] || fail "a Pong match was set out to be reported"
has_results 3 || fail "a Pong match was reported (have $(results))"

echo "12. two fobs check in, play, and the result goes without an operator"
# The hub's part of zaehlwerk-firmware#63, as curls: a fob, a half, once,
# under the hub's source and counter. Pong is stopped first, so attract mode
# does not take the table in between.
kill "$pong_pid" 2>/dev/null || true
wait "$pong_pid" 2>/dev/null || true
# checkin <event_id> <half> <uid> [token] — the status code; the answer is
# left in checkin.json. The counter is passed in because $(…) runs in a
# subshell. The hub sends its token as a bearer (#113); a fourth argument
# sends another one instead, "" none at all.
checkin() {
  local token=${4-$HUB_TOKEN}
  curl -s -o "$WORK/checkin.json" -w '%{http_code}' -X POST "$ZW/ingest/checkin" \
    -H 'content-type: application/json' ${token:+-H "authorization: Bearer $token"} \
    -d "{\"source\":\"e2e-hub\",\"event_id\":$1,\"half\":\"$2\",\"uid\":\"$3\"}"
}
checkins() { curl -sf "$SP/checkins" | jq length; }
outcome() { jq -r .outcome "$WORK/checkin.json"; }

# Anybody else on the network: Anna's UID is no secret, the token is.
[ "$(checkin 1 left 04:a3:1b:22 "")" = 401 ] ||
  fail "a check-in without the hub's token was not refused: $(cat "$WORK/checkin.json")"
[ "$(checkin 1 left 04:a3:1b:22 not-the-hub)" = 401 ] ||
  fail "a check-in with a wrong token was not refused: $(cat "$WORK/checkin.json")"
[ "$(checkins)" = 0 ] || fail "a refused check-in reached Schmetterpause ($(checkins) check-ins over there)"

[ "$(checkin 1 left 04:a3:1b:22)" = 200 ] && [ "$(outcome)" = claimed ] ||
  fail "Anna's fob did not claim the left half: $(cat "$WORK/checkin.json")"
[ "$(checkin 1 left 04:a3:1b:22)" = 200 ] && [ "$(jq -r .repeated "$WORK/checkin.json")" = true ] ||
  fail "the retried check-in was not answered as a repeat: $(cat "$WORK/checkin.json")"
[ "$(checkins)" = 1 ] || fail "a retried check-in was resolved again ($(checkins) check-ins over there)"
[ "$(checkin 2 right DE:AD:BE:EF)" = 404 ] && [ "$(outcome)" = unknown ] ||
  fail "an unknown fob was not answered as one: $(cat "$WORK/checkin.json")"
[ "$(curl -sf "$ZW/checkin" | jq -r '.left.name + "|" + (.right.name // "")')" = "Anna|" ] ||
  fail "the halves are not Anna and free: $(curl -sf "$ZW/checkin")"
[ "$(checkin 3 right 04-5C-7E-91)" = 200 ] && [ "$(outcome)" = started ] ||
  fail "Bernd's fob did not start the match: $(cat "$WORK/checkin.json")"
match=$(jq -r .match_id "$WORK/checkin.json")
[ "$(curl -sf "$ZW/matches/current" | jq -r '.match_id + "|" + .phase + "|" + (.players | join(","))')" = "$match|serve_rally|Anna,Bernd" ] ||
  fail "the checked-in match is not Anna against Bernd playing for the serve: $(curl -sf "$ZW/matches/current")"
before=$(checkins)
[ "$(checkin 4 left "04 11 22 33 44 55 66")" = 409 ] && [ "$(outcome)" = refused ] ||
  fail "a check-in during the match was not refused: $(cat "$WORK/checkin.json")"
[ "$(checkins)" = "$before" ] || fail "a check-in during the match reached Schmetterpause"

# The serve rally, then Anna wins every point at the left half.
presses=0
while [ "$(curl -sf "$ZW/matches/$match" | jq -r .complete)" = false ] && [ $presses -lt 60 ]; do
  presses=$((presses + 1))
  curl -sf -X POST "$ZW/ingest/button" -H 'content-type: application/json' \
    -d "{\"events\":[{\"source\":\"e2e-checkin-left\",\"half\":\"left\",\"event_id\":$presses}]}" >/dev/null ||
    fail "press $presses at the left half was refused"
done
wait_until 10 has_results 4 || fail "the checked-in result did not arrive (have $(results))"
last=$(curl -sf "$SP/results" | jq -c '.[-1]')
[ "$(jq -r '[.home, .away, .entered_via, .operator] | join("|")' <<<"$last")" = "Anna|Bernd|checkin|" ] ||
  fail "the checked-in result arrived as $last"
[ "$(jq -r '.home_checkin_id != null and .away_checkin_id != null' <<<"$last")" = true ] ||
  fail "the checked-in result has no check-ins: $last"
[ "$(curl -sf "$SP/checkins" | jq '[.[] | select(.used)] | length')" = 2 ] ||
  fail "the result did not use exactly the two check-ins"
for log in zw sp; do
  for spelling in de:ad:be:ef DE:AD:BE:EF deadbeef DEADBEEF 04:a3:1b:22 04:A3:1B:22 04-5C-7E-91 "04 11 22 33"; do
    ! grep -qF "$spelling" "$WORK/$log.log" || fail "a UID ($spelling) is in $log.log"
  done
done
echo "   a check-in without the hub's token, or with another, refused with 401 and never resolved"
echo "   match $match: Anna and Bernd by fob, $presses presses, $(jq -c .sets <<<"$last") reported with two check-ins and no operator"

echo "ok: 4 results, $resent resends, $duplicates duplicates, retry after refuse," \
  "an undone match point withheld," \
  "$games Pong games with no unexplained divergence, ends changed at five in the decider," \
  "one of them on two devices," \
  "2 button checks, a match started by two fobs"
