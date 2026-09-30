package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// The board is zaehlwerk-firmware#41 written as a program: join the running
// match, push one ingest event per rally, and take the score from the answer
// rather than counting it. It is the reference client for that issue as much as
// it is a mock — what it sends is what a board should send, and nothing here
// knows anything the API does not tell it.
//
// What it does not do is judge a rally. How a board turns a bounce sequence
// into a verdict is lib/game in the firmware; this only has to produce the
// traffic that verdict would cause.

// joinPoll is how often a board with no match asks again. A person is walking
// from the page to the table in the meantime; a second is plenty.
const joinPoll = time.Second

type boardConfig struct {
	API    string
	Source string
	Pace   time.Duration
	// Ambiguous is the share of rallies sent as delta 0: a hit the board heard
	// but could not attribute, which it reports rather than guesses (ADR-0002).
	Ambiguous float64
	// Resend is the share of events sent twice, as an unacknowledged send is.
	Resend float64
	// Undo is the share of points taken back, as the correction button does.
	Undo    float64
	Seed    int64
	Matches int // matches to play before exiting; 0 keeps going
	// Join waits for a match started on the page, which is all a real board
	// ever does. Unset, the mock starts one itself through the page's form.
	Join   bool
	BestOf int
	// Roster is where the players come from when the mock starts the match:
	// the Schmetterpause zaehlwerk reports to. Nil starts a match with no
	// players named, which is scored and reported nowhere.
	Roster Roster
}

// Roster is the half of the Schmetterpause client the board needs. Asked, never
// assumed: against the fake the names are fixed, against a real instance —
// a preview with its seed data — they are whatever it holds, and a match meant
// to be reported can only be started with ids that instance knows (ADR-0004).
type Roster interface {
	Players(ctx context.Context) ([]schmetterpause.Player, error)
	Operators(ctx context.Context) ([]schmetterpause.Operator, error)
}

// lineup is what a person picks on the page: two players and whoever keeps
// score.
type lineup struct {
	home, away schmetterpause.Player
	operator   schmetterpause.Operator
}

type board struct {
	cfg  boardConfig
	http *http.Client
	log  *slog.Logger
	rnd  *rand.Rand

	// eventID only goes up. A board keeps it in NVS so a reboot does not
	// replay event 1 into a match that has already seen it; the mock has no
	// NVS and writes no file, so it starts from the clock, which a restart
	// also cannot send backwards.
	eventID uint64
}

func newBoard(cfg boardConfig, log *slog.Logger) *board {
	return &board{
		cfg:     cfg,
		http:    &http.Client{Timeout: 5 * time.Second},
		log:     log,
		rnd:     rand.New(rand.NewSource(cfg.Seed)),
		eventID: uint64(time.Now().UnixMilli()),
	}
}

// errMatchOver ends a match from the board's side: won, ended on the page, or
// replaced by a newer one.
var errMatchOver = errors.New("the match is over")

func (b *board) run(ctx context.Context) error {
	for played := 0; b.cfg.Matches == 0 || played < b.cfg.Matches; played++ {
		var picked *lineup
		if !b.cfg.Join {
			var err error
			if picked, err = b.create(ctx); err != nil {
				return err
			}
		}

		st, err := b.join(ctx)
		if err != nil {
			return err
		}
		if picked != nil && st.Players != [2]string{picked.home.DisplayName, picked.away.DisplayName} {
			// zaehlwerk names the players from its own Schmetterpause. If the
			// names differ, the two are not the same instance, and the result
			// would be refused at the end of the match for ids it does not
			// know — better said now than after a best of five.
			return fmt.Errorf("zaehlwerk named the players %q, the roster %q and %q: "+
				"is it reporting to the same Schmetterpause as SCHMETTERPAUSE_URL here?",
				st.Players, picked.home.DisplayName, picked.away.DisplayName)
		}
		if err := b.play(ctx, st); err != nil {
			return err
		}
	}
	return nil
}

// create posts the page's new-match form. Scraping a page is the price of the
// only door there is: the JSON API names players as free text and starts a
// match nobody reports (ADR-0004), so a match meant to reach Schmetterpause is
// started where a person would start it.
func (b *board) create(ctx context.Context) (*lineup, error) {
	form := url.Values{"best_of": {strconv.Itoa(b.cfg.BestOf)}}

	var picked *lineup
	if b.cfg.Roster != nil {
		l, err := b.pick(ctx)
		if err != nil {
			return nil, err
		}
		picked = &l
		form.Set("home_id", l.home.ID)
		form.Set("away_id", l.away.ID)
		form.Set("operator_id", l.operator.ID)
		b.log.Info("starting a match", "home", l.home.DisplayName, "away", l.away.DisplayName,
			"operator", l.operator.DisplayName, "best_of", b.cfg.BestOf)
	} else {
		b.log.Info("starting a match nobody reports, no SCHMETTERPAUSE_URL", "best_of", b.cfg.BestOf)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.API+"/ui/matches",
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("building the new-match form: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("posting the new-match form: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	page, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// The page answers a bad form with 200 and the reason in the markup,
	// because htmx swaps it in where the form was.
	if m := pageError.FindSubmatch(page); m != nil {
		return nil, fmt.Errorf("the page did not start the match: %s", html.UnescapeString(string(m[1])))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("posting the new-match form: %s", resp.Status)
	}
	return picked, nil
}

// pick chooses two players and somebody to keep score, from whatever the
// roster holds. An observer counts if there is one, because that is what an
// observer is for; otherwise anybody not playing. The seed decides, so a run
// can be repeated against the same data.
func (b *board) pick(ctx context.Context) (lineup, error) {
	players, err := b.cfg.Roster.Players(ctx)
	if err != nil {
		return lineup{}, fmt.Errorf("reading the players: %w", err)
	}
	if len(players) < 2 {
		return lineup{}, fmt.Errorf("the roster has %d players, a match needs two", len(players))
	}
	order := b.rnd.Perm(len(players))
	l := lineup{home: players[order[0]], away: players[order[1]]}

	operators, err := b.cfg.Roster.Operators(ctx)
	if err != nil {
		return lineup{}, fmt.Errorf("reading who may keep score: %w", err)
	}
	var free []schmetterpause.Operator
	for _, o := range operators {
		if o.ID != l.home.ID && o.ID != l.away.ID {
			free = append(free, o)
		}
	}
	if len(free) == 0 {
		return lineup{}, errors.New("nobody but the two players may keep score, and they may not")
	}
	b.rnd.Shuffle(len(free), func(i, j int) { free[i], free[j] = free[j], free[i] })
	l.operator = free[0]
	for _, o := range free {
		if o.Observer {
			l.operator = o
			break
		}
	}
	return l, nil
}

var pageError = regexp.MustCompile(`<div class="error">([^<]*)</div>`)

// join waits for a running match, as a board does after power-up.
func (b *board) join(ctx context.Context) (scorer.State, error) {
	waiting := false
	for {
		st, status, err := b.call(ctx, http.MethodGet, "/matches/current", nil)
		switch {
		case err != nil:
			return st, err
		case status == http.StatusOK:
			b.log.Info("joined", "match_id", st.MatchID,
				"players", st.Players, "serving", st.Players[index(st.Serving)])
			return st, nil
		case status != http.StatusNotFound:
			return st, fmt.Errorf("GET /matches/current: %d", status)
		}

		if !waiting {
			b.log.Info("no match running, waiting for one to be started on the page")
			waiting = true
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(joinPoll):
		}
	}
}

func (b *board) play(ctx context.Context, st scorer.State) error {
	id := st.MatchID
	for {
		// Asked again before every rally, because the phone may have taken a
		// point back since the last answer, and who serves next is part of
		// how a board judges the rally. A board that remembered would judge
		// it against a serve that no longer holds.
		cur, status, err := b.call(ctx, http.MethodGet, "/matches/current", nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK || cur.MatchID != id {
			final, _, err := b.call(ctx, http.MethodGet, "/matches/"+id, nil)
			if err != nil {
				return err
			}
			b.log.Info("match over", "match_id", id, "sets", final.Sets,
				"completed_sets", final.CompletedSets, "winner", final.Winner)
			return nil
		}

		if err := b.rally(ctx, cur); errors.Is(err, errMatchOver) {
			continue
		} else if err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.cfg.Pace):
		}
	}
}

// ingestBody is the ADR-0002 event, with the match the board joined. Four
// fields and an id, and nothing a sensor measured: peaks and curves go to the
// firmware's log sink, not here.
type ingestBody struct {
	MatchID string `json:"match_id"`
	Source  string `json:"source"`
	Player  string `json:"player"`
	Delta   int    `json:"delta"`
	EventID uint64 `json:"event_id"`
}

func (b *board) rally(ctx context.Context, st scorer.State) error {
	half := "A"
	if b.rnd.Intn(2) == 1 {
		half = "B"
	}
	player := playerOn(half, st.SetNumber)

	delta := 1
	if b.rnd.Float64() < b.cfg.Ambiguous {
		delta = 0
	}

	b.eventID++
	ev := ingestBody{MatchID: st.MatchID, Source: b.cfg.Source,
		Player: string(player), Delta: delta, EventID: b.eventID}

	after, err := b.ingest(ctx, ev)
	if err != nil {
		return err
	}
	b.log.Info("rally", "half", half, "player", st.Players[index(player)], "delta", delta,
		"event_id", ev.EventID, "points", after.Points, "sets", after.Sets,
		"serving", after.Players[index(after.Serving)])

	if b.rnd.Float64() < b.cfg.Resend {
		again, err := b.ingest(ctx, ev)
		if err != nil {
			return err
		}
		// The whole point of the contract. If this fires, zaehlwerk counted
		// one rally twice and a set can go to the wrong player.
		if again.Points != after.Points || again.Sets != after.Sets {
			return fmt.Errorf("event %d sent again changed the score from %v %v to %v %v",
				ev.EventID, after.Points, after.Sets, again.Points, again.Sets)
		}
		b.log.Info("resent", "event_id", ev.EventID, "points", again.Points)
	}

	if delta == 1 && !after.Complete && b.rnd.Float64() < b.cfg.Undo {
		back, status, err := b.call(ctx, http.MethodPost, "/matches/"+st.MatchID+"/undo", nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("POST /matches/%s/undo: %d", st.MatchID, status)
		}
		b.log.Info("correction", "points", back.Points, "sets", back.Sets)
	}
	return nil
}

func (b *board) ingest(ctx context.Context, ev ingestBody) (scorer.State, error) {
	st, status, err := b.call(ctx, http.MethodPost, "/ingest/piezo", ev)
	switch {
	case err != nil:
		return st, err
	case status == http.StatusOK:
		return st, nil
	case status == http.StatusConflict || status == http.StatusNotFound:
		// Won by the previous rally, or ended on the page between two.
		return st, errMatchOver
	}
	return st, fmt.Errorf("POST /ingest/piezo: %d", status)
}

// call sends a request and decodes the state, from the body or from the state
// an error body carries. The status is returned rather than judged here,
// because a 404 means "wait" to join and "over" to a rally.
func (b *board) call(ctx context.Context, method, path string, body any) (scorer.State, int, error) {
	var st scorer.State

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return st, 0, fmt.Errorf("encoding %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.cfg.API+path, reader)
	if err != nil {
		return st, 0, fmt.Errorf("building %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := b.http.Do(req)
	if err != nil {
		return st, 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return st, resp.StatusCode, fmt.Errorf("reading %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, &st); err != nil {
			return st, resp.StatusCode, fmt.Errorf("decoding %s %s: %w", method, path, err)
		}
		return st, resp.StatusCode, nil
	}

	var refusal struct {
		Error string        `json:"error"`
		State *scorer.State `json:"state"`
	}
	if json.Unmarshal(raw, &refusal) == nil {
		if refusal.State != nil {
			st = *refusal.State
		}
		b.log.Debug("refused", "method", method, "path", path,
			"status", resp.StatusCode, "error", refusal.Error)
	}
	return st, resp.StatusCode, nil
}

// playerOn resolves a table half to the player standing there
// (zaehlwerk-firmware ADR-0006). A sensor is under a half and stays there; the
// players change ends after every set. On a board that change is a button
// somebody presses — the mock presses it at every set boundary and leaves out
// the change at five points in a deciding set.
func playerOn(half string, setNumber int) scorer.Player {
	aOnHalfA := setNumber%2 == 1
	if (half == "A") == aOnHalfA {
		return scorer.PlayerA
	}
	return scorer.PlayerB
}

func index(p scorer.Player) int {
	if p == scorer.PlayerB {
		return 1
	}
	return 0
}
