package checkin

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// Fobs of the fake roster, written the way a reader might.
const (
	annaFob  = "04:A3:1B:22"
	berndFob = "04-5C-7E-91"
	claraFob = "04 11 22 33 44 55 66"
	// strangerFob is a badge held up by mistake: nobody owns it.
	strangerFob = "DE:AD:BE:EF"
)

// fakeSP resolves the fobs above and counts how often it was asked, which is
// how many check-ins it would have recorded.
type fakeSP struct {
	mu       sync.Mutex
	owners   map[string]string // normalised uid -> player id
	names    map[string]string
	resolves int
	n        int
	// err, when set, is the answer to every resolve.
	err error
	// gate, when set, holds every resolve until it is closed.
	gate chan struct{}
	// players, when set, holds every read of the player list until it is
	// closed: the one call a phone claim makes.
	players chan struct{}
}

func newFakeSP() *fakeSP {
	return &fakeSP{
		owners: map[string]string{
			"04:A3:1B:22":          "anna",
			"04:5C:7E:91":          "bernd",
			"04:11:22:33:44:55:66": "clara",
		},
		names: map[string]string{"anna": "Anna", "bernd": "Bernd", "clara": "Clara"},
	}
}

func (f *fakeSP) ResolveFob(_ context.Context, uid, _ string) (schmetterpause.FobCheckin, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++
	if f.err != nil {
		return schmetterpause.FobCheckin{}, f.err
	}
	id, ok := f.owners[uid]
	if !ok {
		return schmetterpause.FobCheckin{}, schmetterpause.ErrUnknownFob
	}
	f.n++
	return schmetterpause.FobCheckin{PlayerID: id, CheckinID: id + "-checkin-" + string(rune('0'+f.n))}, nil
}

func (f *fakeSP) Players(context.Context) ([]schmetterpause.Player, error) {
	if f.players != nil {
		<-f.players
	}
	var out []schmetterpause.Player
	for id, name := range f.names {
		out = append(out, schmetterpause.Player{ID: id, DisplayName: name})
	}
	return out, nil
}

func (f *fakeSP) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolves
}

// registryTable is the table over a real registry, as the API server is.
type registryTable struct{ r *match.Registry }

func (t registryTable) Occupied() bool {
	_, ok := t.r.Occupied()
	return ok
}

func (t registryTable) StartMatch(cfg scorer.Config, h match.Handover) (*match.Match, error) {
	m, _, err := t.r.CreateFor(cfg, match.Setup{Handover: h})
	return m, err
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type harness struct {
	desk  *Desk
	sp    *fakeSP
	table registryTable
	clock *clock
	logs  *bytes.Buffer
	ids   map[string]uint64
}

func newHarness(t *testing.T, opts ...Option) *harness {
	t.Helper()
	h := &harness{
		sp:    newFakeSP(),
		table: registryTable{match.NewRegistry()},
		clock: &clock{now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)},
		logs:  &bytes.Buffer{},
		ids:   map[string]uint64{},
	}
	log := slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.desk = NewDesk(h.sp, append([]Option{WithClock(h.clock.Now), WithLogger(log)}, opts...)...)
	return h
}

// checkIn is the hub: a fob, then a press at half, under the hub's source
// with the next event id.
func (h *harness) checkIn(t *testing.T, uid string, half scorer.Half) Answer {
	t.Helper()
	h.ids["hub"]++
	return h.send(t, Request{Source: "hub", EventID: h.ids["hub"], Half: half, UID: uid})
}

func (h *harness) send(t *testing.T, req Request) Answer {
	t.Helper()
	require.NoError(t, req.Validate())
	return h.desk.CheckIn(context.Background(), req, h.table)
}

func TestTwoClaimsStartTheMatchWithLeftAsA(t *testing.T) {
	h := newHarness(t)

	a := h.checkIn(t, berndFob, scorer.HalfRight)
	require.Equal(t, Claimed, a.Outcome)
	require.Equal(t, "Bernd", a.Name)

	a = h.checkIn(t, annaFob, scorer.HalfLeft)
	require.Equal(t, Started, a.Outcome)
	require.NotEmpty(t, a.MatchID)
	require.Equal(t, [2]string{"", ""}, a.Halves, "both claims are used up by the match")

	m, ok := h.table.r.Occupied()
	require.True(t, ok)
	require.Equal(t, a.MatchID, m.ID)
	require.Equal(t, match.Handover{
		HomeID: "anna", AwayID: "bernd",
		HomeCheckinID: "anna-checkin-2", AwayCheckinID: "bernd-checkin-1",
	}, m.Handover, "left is a and home, and no operator")
	require.True(t, m.Handover.Wanted())

	st := m.Scorer.State()
	require.Equal(t, [2]string{"Anna", "Bernd"}, st.Players)
	require.Equal(t, scorer.PhaseServeRally, st.Phase, "a checked-in match plays for the serve")
}

func TestACheckedInMatchStartsInTheConfiguredPhase(t *testing.T) {
	h := newHarness(t, WithMatch(scorer.Config{Start: scorer.StartWarmup, ChangeEnds: true}))

	h.checkIn(t, annaFob, scorer.HalfLeft)
	a := h.checkIn(t, berndFob, scorer.HalfRight)
	require.Equal(t, Started, a.Outcome)

	m, ok := h.table.r.Occupied()
	require.True(t, ok)
	require.Equal(t, scorer.PhaseWarmup, m.Scorer.State().Phase)
}

func TestALoneClaimTimesOutAndReleasesTheHalf(t *testing.T) {
	h := newHarness(t, WithTimeout(time.Minute))

	require.Equal(t, Claimed, h.checkIn(t, annaFob, scorer.HalfLeft).Outcome)
	require.NotNil(t, h.desk.Claims()[0])

	h.clock.advance(59 * time.Second)
	require.NotNil(t, h.desk.Claims()[0], "not yet")

	h.clock.advance(time.Second)
	require.Nil(t, h.desk.Claims()[0], "the half is free again")
	require.Contains(t, h.logs.String(), "check-in timed out")

	// And the next pair starts from nothing: Bernd alone does not start a
	// match with Anna's expired claim.
	require.Equal(t, Claimed, h.checkIn(t, berndFob, scorer.HalfRight).Outcome)
	_, ok := h.table.r.Occupied()
	require.False(t, ok)
}

func TestALoneClaimIsReleasedWithoutAnybodyAsking(t *testing.T) {
	h := newHarness(t, WithTimeout(20*time.Millisecond))
	// The real clock here: the timer sweeps on its own.
	h.desk.now = time.Now

	h.checkIn(t, annaFob, scorer.HalfLeft)
	require.Eventually(t, func() bool {
		return strings.Contains(syncString(h), "check-in timed out")
	}, time.Second, 5*time.Millisecond)
}

// syncString reads the log under the desk's lock, which every write to it
// holds.
func syncString(h *harness) string {
	h.desk.mu.Lock()
	defer h.desk.mu.Unlock()
	return h.logs.String()
}

func TestAClaimDuringARunningMatchIsRefusedAndChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.checkIn(t, claraFob, scorer.HalfLeft)

	running, err := h.table.r.Create(scorer.Config{})
	require.NoError(t, err)
	before := h.sp.calls()

	a := h.checkIn(t, annaFob, scorer.HalfRight)
	require.Equal(t, Refused, a.Outcome)
	require.Error(t, a.Err)
	require.Equal(t, before, h.sp.calls(), "schmetterpause is not asked, so no check-in is made there")

	claims := h.desk.Claims()
	require.Equal(t, "Clara", claims[0].Name, "the claim from before stays")
	require.Nil(t, claims[1])
	current, err := h.table.r.Current()
	require.NoError(t, err)
	require.Same(t, running, current)
}

func TestADemoDoesNotStopACheckIn(t *testing.T) {
	h := newHarness(t)
	demo, _, err := h.table.r.CreateFor(scorer.Config{}, match.Setup{Demo: true})
	require.NoError(t, err)

	h.checkIn(t, annaFob, scorer.HalfLeft)
	a := h.checkIn(t, berndFob, scorer.HalfRight)
	require.Equal(t, Started, a.Outcome)
	require.False(t, demo.Running(), "the checked-in match took the table from the demo")
}

func TestTheSamePlayerAtTheSameHalfChangesNothing(t *testing.T) {
	h := newHarness(t, WithTimeout(time.Minute))
	h.checkIn(t, annaFob, scorer.HalfLeft)
	h.clock.advance(50 * time.Second)

	a := h.checkIn(t, annaFob, scorer.HalfLeft)
	require.Equal(t, Unchanged, a.Outcome)
	require.Equal(t, [2]string{"Anna", ""}, a.Halves)

	// The newer check-in is kept, and it counts from now.
	h.clock.advance(50 * time.Second)
	require.NotNil(t, h.desk.Claims()[0])
	require.Equal(t, "anna-checkin-2", h.desk.claims[0].CheckinID)
}

func TestAnotherPlayerReplacesTheClaim(t *testing.T) {
	h := newHarness(t)
	h.checkIn(t, annaFob, scorer.HalfLeft)

	a := h.checkIn(t, claraFob, scorer.HalfLeft)
	require.Equal(t, Replaced, a.Outcome)
	require.Equal(t, [2]string{"Clara", ""}, a.Halves)
}

func TestTheSamePlayerAtTheOtherHalfMoves(t *testing.T) {
	h := newHarness(t)
	h.checkIn(t, annaFob, scorer.HalfLeft)

	a := h.checkIn(t, annaFob, scorer.HalfRight)
	require.Equal(t, Moved, a.Outcome)
	require.Equal(t, [2]string{"", "Anna"}, a.Halves)

	_, ok := h.table.r.Occupied()
	require.False(t, ok, "one player at two halves is no match")
}

func TestARetriedCheckInClaimsOnceAndResolvesOnce(t *testing.T) {
	h := newHarness(t)
	req := Request{Source: "hub", EventID: 7, Half: scorer.HalfLeft, UID: annaFob}

	first := h.send(t, req)
	second := h.send(t, req)

	require.Equal(t, 1, h.sp.calls(), "every resolve is a check-in over there")
	require.Equal(t, Claimed, first.Outcome)
	require.Equal(t, Claimed, second.Outcome, "the retry gets the first answer")
	require.True(t, second.Repeated)
	require.False(t, first.Repeated)
}

func TestARetryWhileTheFirstIsUnderWayWaitsForIt(t *testing.T) {
	h := newHarness(t)
	h.sp.gate = make(chan struct{})
	req := Request{Source: "hub", EventID: 3, Half: scorer.HalfRight, UID: berndFob}
	require.NoError(t, req.Validate())

	answers := make(chan Answer, 2)
	for range 2 {
		go func() { answers <- h.desk.CheckIn(context.Background(), req, h.table) }()
	}
	// Both are in: one resolving, one waiting for it.
	time.Sleep(20 * time.Millisecond)
	close(h.sp.gate)

	a, b := <-answers, <-answers
	require.Equal(t, 1, h.sp.calls())
	require.Equal(t, Claimed, a.Outcome)
	require.Equal(t, Claimed, b.Outcome)
	require.NotEqual(t, a.Repeated, b.Repeated, "one is the attempt, the other its repeat")
}

func TestAnOlderEventIDIsADuplicate(t *testing.T) {
	h := newHarness(t)
	h.send(t, Request{Source: "hub", EventID: 5, Half: scorer.HalfLeft, UID: annaFob})

	a := h.send(t, Request{Source: "hub", EventID: 4, Half: scorer.HalfRight, UID: berndFob})
	require.Equal(t, Duplicate, a.Outcome)
	require.Equal(t, 1, h.sp.calls())
	require.Nil(t, h.desk.Claims()[1])
}

func TestARestartedHubIsHeardAfterAQuietMinute(t *testing.T) {
	h := newHarness(t, WithTimeout(time.Hour))
	h.send(t, Request{Source: "hub", EventID: 40, Half: scorer.HalfLeft, UID: annaFob})

	h.clock.advance(scorer.DefaultRestartQuietPeriod)
	a := h.send(t, Request{Source: "hub", EventID: 1, Half: scorer.HalfRight, UID: berndFob})
	require.Equal(t, Started, a.Outcome)
}

func TestSourcesCountForThemselves(t *testing.T) {
	h := newHarness(t)
	h.send(t, Request{Source: "hub-1", EventID: 9, Half: scorer.HalfLeft, UID: annaFob})
	a := h.send(t, Request{Source: "hub-2", EventID: 1, Half: scorer.HalfRight, UID: berndFob})
	require.Equal(t, Started, a.Outcome)
}

func TestAnUnknownFobChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.checkIn(t, annaFob, scorer.HalfLeft)

	a := h.checkIn(t, strangerFob, scorer.HalfRight)
	require.Equal(t, Unknown, a.Outcome)
	require.Equal(t, [2]string{"Anna", ""}, a.Halves)
}

func TestAnOutageFailsTheCheckInAndTheHalfStaysFree(t *testing.T) {
	h := newHarness(t)
	h.sp.err = errors.New("dial tcp: connection refused")

	a := h.checkIn(t, annaFob, scorer.HalfLeft)
	require.Equal(t, Unavailable, a.Outcome)
	require.ErrorContains(t, a.Err, "connection refused")
	require.Nil(t, h.desk.Claims()[0])

	// The player holds the fob up again once it is back.
	h.sp.err = nil
	require.Equal(t, Claimed, h.checkIn(t, annaFob, scorer.HalfLeft).Outcome)
}

func TestAMissingRouteFailsTheCheckIn(t *testing.T) {
	h := newHarness(t)
	h.sp.err = schmetterpause.ErrNoFobRoute

	a := h.checkIn(t, annaFob, scorer.HalfLeft)
	require.Equal(t, Unavailable, a.Outcome)
	require.ErrorIs(t, a.Err, schmetterpause.ErrNoFobRoute)
}

// The UID is a card number, and a badge held up by mistake is somebody's.
// It is in no log line, no answer and nothing the desk keeps, whatever
// happened to the check-in.
func TestAUIDAppearsNowhere(t *testing.T) {
	h := newHarness(t)
	var answers []Answer

	answers = append(answers, h.checkIn(t, strangerFob, scorer.HalfLeft))
	answers = append(answers, h.send(t, Request{Source: "hub", EventID: h.ids["hub"], Half: scorer.HalfLeft, UID: strangerFob}))
	h.sp.err = errors.New("boom")
	answers = append(answers, h.checkIn(t, strangerFob, scorer.HalfRight))
	h.sp.err = nil
	answers = append(answers, h.checkIn(t, annaFob, scorer.HalfLeft))
	_, err := h.table.r.Create(scorer.Config{})
	require.NoError(t, err)
	answers = append(answers, h.checkIn(t, strangerFob, scorer.HalfRight))

	var seen strings.Builder
	seen.WriteString(h.logs.String())
	for _, a := range answers {
		seen.WriteString(string(a.Outcome) + a.Name + a.MatchID + strings.Join(a.Halves[:], ","))
		if a.Err != nil {
			seen.WriteString(a.Err.Error())
		}
	}
	for _, src := range h.desk.sources {
		seen.WriteString(src.last.answer.Name)
		if src.last.answer.Err != nil {
			seen.WriteString(src.last.answer.Err.Error())
		}
	}
	for _, c := range h.desk.claims {
		if c != nil {
			seen.WriteString(c.PlayerID + c.CheckinID + c.Name)
		}
	}

	requireNoUID(t, seen.String(), strangerFob)
	requireNoUID(t, seen.String(), annaFob)
}

// requireNoUID looks for the UID's bytes in any spelling: with colons,
// dashes, spaces or none, in either case.
func requireNoUID(t *testing.T, haystack, uid string) {
	t.Helper()
	norm, err := NormaliseUID(uid)
	require.NoError(t, err)
	parts := strings.Split(norm, ":")
	lower := strings.ToLower(haystack)
	for _, sep := range []string{":", "-", " ", ""} {
		require.NotContains(t, lower, strings.ToLower(strings.Join(parts, sep)), "the UID leaked")
	}
}

func TestUIDsAreCheckedTheWayAReaderWritesThem(t *testing.T) {
	for in, want := range map[string]string{
		"04:a3:1b:22":                   "04:A3:1B:22",
		"04-A3-1B-22":                   "04:A3:1B:22",
		"04 a3 1b 22":                   "04:A3:1B:22",
		"04a31b22":                      "04:A3:1B:22",
		"04:11:22:33:44:55:66":          "04:11:22:33:44:55:66",
		"01:02:03:04:05:06:07:08:09:0a": "01:02:03:04:05:06:07:08:09:0A",
	} {
		got, err := NormaliseUID(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got)
	}
	for _, bad := range []string{"", "04:A3:1B", "04:A3:1B:22:33", "zz:a3:1b:22", "04:A3:1B:2"} {
		_, err := NormaliseUID(bad)
		require.ErrorIs(t, err, ErrBadUID, bad)
		if bad != "" {
			require.NotContains(t, err.Error(), bad)
		}
	}
}

func TestARequestIsValidatedWithoutQuotingTheUID(t *testing.T) {
	req := Request{Source: "hub", Half: scorer.HalfLeft, UID: "04:A3:1B:ZZ"}
	err := req.Validate()
	require.ErrorIs(t, err, ErrBadUID)
	require.NotContains(t, err.Error(), "A3")

	req = Request{Half: scorer.HalfLeft, UID: annaFob}
	require.ErrorContains(t, req.Validate(), "source")

	req = Request{Source: "hub", Half: "middle", UID: annaFob}
	require.ErrorContains(t, req.Validate(), "half")
}
