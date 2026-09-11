package panel

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	homerun "github.com/stuttgart-things/homerun-library/v4"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// These tests go through a real redis-stack, because the parts that can be
// wrong here are the parts a fake pitcher cannot have an opinion about: whether
// the JSON document lands under a key the catcher can resolve, and whether the
// stream entry points at it.
//
//	docker run -d --name zw-redis -p 6399:6379 redis/redis-stack-server:latest
//	REDIS_TEST_ADDR=localhost:6399 go test ./internal/panel/
//
// Skipped without REDIS_TEST_ADDR so the ordinary `go test ./...` needs no
// container.
func redisAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("set REDIS_TEST_ADDR to a redis-stack to run the panel integration tests")
	}
	return addr
}

func redisClient(t *testing.T, addr string) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, c.Ping(ctx).Err(), "redis at %s", addr)
	return c
}

// pitcherFor builds a real homerun pitcher against a stream of its own, so
// parallel runs of this file do not read each other's points.
func pitcherFor(t *testing.T, addr, stream string) *homerun.Pitcher {
	t.Helper()
	host, port, ok := splitHostPort(addr)
	require.True(t, ok, "REDIS_TEST_ADDR must be host:port, got %q", addr)

	return homerun.NewPitcher(homerun.RedisConfig{Addr: host, Port: port, Stream: stream})
}

func splitHostPort(addr string) (host, port string, ok bool) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], true
		}
	}
	return "", "", false
}

// readAsTheCatcherDoes follows the exact path led_catcher's RedisConsumer takes:
// read the stream entry, take its messageID field, then JSON.GET that key. A
// test that asserted on what the sink handed the pitcher would pass even if the
// two halves did not line up.
func readAsTheCatcherDoes(t *testing.T, c *redis.Client, stream string, want int) []homerun.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var msgs []homerun.Message
	deadline := time.Now().Add(10 * time.Second)
	lastID := "0"

	for len(msgs) < want && time.Now().Before(deadline) {
		entries, err := c.XRead(ctx, &redis.XReadArgs{
			Streams: []string{stream, lastID},
			Count:   64,
			Block:   500 * time.Millisecond,
		}).Result()
		if err == redis.Nil {
			continue
		}
		require.NoError(t, err)

		for _, s := range entries {
			for _, e := range s.Messages {
				lastID = e.ID

				id, ok := e.Values["messageID"].(string)
				require.True(t, ok, "stream entry %s carries no messageID: %v", e.ID, e.Values)

				raw, err := c.Do(ctx, "JSON.GET", id, "$").Text()
				require.NoError(t, err, "JSON.GET %s — the stream points at a document that is not there", id)

				var decoded []homerun.Message
				require.NoError(t, json.Unmarshal([]byte(raw), &decoded))
				require.Len(t, decoded, 1)
				msgs = append(msgs, decoded[0])
			}
		}
	}

	require.Len(t, msgs, want, "expected %d messages on %s", want, stream)
	return msgs
}

func TestAgainstRedisTheCatcherCanResolveWhatWePublish(t *testing.T) {
	addr := redisAddr(t)
	stream := "zw-test-resolve"
	c := redisClient(t, addr)
	require.NoError(t, c.Del(context.Background(), stream).Err())

	sink := New(pitcherFor(t, addr, stream), Config{Stream: stream, Logger: quiet()})
	sc := newScorer(t, sink)
	point(t, sc, scorer.PlayerA, 1)
	point(t, sc, scorer.PlayerB, 2)
	require.NoError(t, sink.Close())

	msgs := readAsTheCatcherDoes(t, c, stream, 2)

	require.Equal(t, "1:0", msgs[0].Title)
	require.Equal(t, "1:1", msgs[1].Title)
	require.Equal(t, "match=m1,set=1,transition=point,side=a", msgs[0].Tags)
	require.Equal(t, "match=m1,set=1,transition=point,side=b", msgs[1].Tags)
	for _, m := range msgs {
		require.Equal(t, "tabletennis", m.System)
		require.Equal(t, "INFO", m.Severity)
		require.Equal(t, "zaehlwerk", m.Author)

		_, err := time.Parse(time.RFC3339, m.Timestamp)
		require.NoError(t, err, "timestamp %q is not RFC3339", m.Timestamp)
	}
}

// The panel path for a whole match: every transition, in order, on the stream,
// with the set and match wins carrying the severity the profile colours
// differently.
func TestAgainstRedisAWholeMatchArrivesInOrder(t *testing.T) {
	addr := redisAddr(t)
	stream := "zw-test-match"
	c := redisClient(t, addr)
	require.NoError(t, c.Del(context.Background(), stream).Err())

	sink := New(pitcherFor(t, addr, stream), Config{Stream: stream, Logger: quiet()})

	sc, err := scorer.New(scorer.Config{MatchID: "m1", Players: [2]string{"Anna", "Bernd"}, BestOf: 3})
	require.NoError(t, err)

	var expected []homerun.Message
	sc.Observe(func(tr scorer.Transition) {
		expected = append(expected, Message(tr, DefaultSystem, DefaultAuthor, time.Now()))
	})
	sc.Observe(sink.Observe)

	for i := uint64(1); !sc.State().Complete; i++ {
		_, err := sc.Apply(scorer.ScoreEvent{
			MatchID: "m1", Player: scorer.PlayerA, Delta: 1, EventID: i, Source: "test",
		})
		require.NoError(t, err)
		require.Less(t, i, uint64(100), "match did not finish")
	}
	require.NoError(t, sink.Close())

	got := readAsTheCatcherDoes(t, c, stream, len(expected))

	for i := range expected {
		require.Equal(t, expected[i].Title, got[i].Title, "message %d", i)
		require.Equal(t, expected[i].Severity, got[i].Severity, "message %d", i)
		require.Equal(t, expected[i].Message, got[i].Message, "message %d", i)
		require.Equal(t, expected[i].Tags, got[i].Tags, "message %d", i)
	}

	require.Equal(t, "WIN 2:0", got[len(got)-1].Title)
	require.Equal(t, "SUCCESS", got[len(got)-1].Severity)
}

// The failure the issue names by name: Redis unreachable must not stall or
// crash the match. Same assertion as the fake-pitcher test, but against a real
// client dialling a port with nothing behind it — a refused connection and a
// blackholed one fail in different ways.
func TestAgainstRedisAnUnreachableRedisDoesNotStopTheMatch(t *testing.T) {
	redisAddr(t) // keeps this with the rest of the integration set

	// Port 1 is reserved and nothing listens on it.
	sink := New(pitcherFor(t, "127.0.0.1:1", "zw-test-unreachable"),
		Config{PitchTimeout: 200 * time.Millisecond, DrainTimeout: 200 * time.Millisecond, Logger: quiet()})
	sc := newScorer(t, sink)

	start := time.Now()
	for i := range uint64(30) {
		point(t, sc, scorer.PlayerA, i+1)
	}
	elapsed := time.Since(start)

	require.Less(t, elapsed, 2*time.Second, "applying points took %s with redis down", elapsed)
	require.Equal(t, 30, sc.State().Points[0]+setPoints(sc.State()))
	require.NoError(t, sink.Close())
	require.Positive(t, sink.Failed()+sink.Dropped(), "nothing was reported as undelivered")
}
