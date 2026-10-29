package pong

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
)

// A series is many headless games in a row with the same faults, to measure
// how often zaehlwerk gets each kind of fault right: one game shows whether a
// fault was handled, a few hundred show how often. Every game plays as fast
// as zaehlwerk answers, takes the service's next seed or the series' own, and
// is a game like any other — listed under /games while the service remembers
// it, and ended like any other. A game that does not end won ends the series.

// maxSeriesGames bounds a series. A best of three is some sixty presses, so
// this is a few minutes against a zaehlwerk on the same machine — or hours
// with counter_reset on, which waits a minute before each.
const maxSeriesGames = 1000

// seriesBody is POST /series.
type seriesBody struct {
	// Games is how many games to play, 1 to maxSeriesGames.
	Games int `json:"games"`
	// Faults is what every game injects; absent is PONG_FAULTS, as for
	// POST /games.
	Faults *Faults `json:"faults"`
	// Seed is the first game's seed, and every next game's one higher: the
	// same series again. Absent is the service's next seeds.
	Seed *int64 `json:"seed"`
}

// SeriesStatus is where a series is.
type SeriesStatus string

const (
	SeriesPlaying SeriesStatus = "playing"
	// SeriesDone is every game played.
	SeriesDone SeriesStatus = "done"
	// SeriesEnded is a series that stopped early: a game that did not end
	// won, a game that did not start, or the service shutting down.
	SeriesEnded SeriesStatus = "ended"
)

// SeriesView is a series as GET /series/{id} shows it.
type SeriesView struct {
	ID     string       `json:"id"`
	Status SeriesStatus `json:"status"`
	Reason string       `json:"reason,omitempty"`
	Games  int          `json:"games"`
	// Played is the games that ended won; Clean those of them without an
	// unexplained divergence.
	Played      int    `json:"played"`
	Clean       int    `json:"clean"`
	FaultShares Faults `json:"fault_shares"`
	// Kinds is each fault the series injects: how often, and how often
	// zaehlwerk did not do what it must.
	Kinds []KindTally `json:"kinds"`
	// Unattributed is the findings at a press without a fault: zaehlwerk
	// wrong where the table sent nothing wrong, or what an earlier finding
	// set off — a reset zaehlwerk discarded has it discard every press
	// after it, as duplicates.
	Unattributed int `json:"unattributed"`
	// Findings is each game with a finding, to play again by its seed.
	Findings []SeriesFinding `json:"findings"`
	// Game is the game playing now, empty once the series is over.
	Game string `json:"game,omitempty"`
}

// KindTally is one kind of fault over a series.
type KindTally struct {
	Kind     FaultKind `json:"kind"`
	Injected int       `json:"injected"`
	// Findings is the unexplained divergences at a press this fault was
	// injected at.
	Findings int `json:"findings"`
	// Accuracy is the share of the injected faults zaehlwerk handled as it
	// must, 1 until one has been injected.
	Accuracy float64 `json:"accuracy"`
}

// SeriesFinding is a game of a series with a finding.
type SeriesFinding struct {
	Game        string `json:"game"`
	Seed        int64  `json:"seed"`
	Unexplained int    `json:"unexplained"`
}

// series is one series' record. Guarded by mu; only its own goroutine plays.
type series struct {
	id     string
	games  int
	faults Faults

	mu           sync.Mutex
	status       SeriesStatus
	reason       string
	played       int
	clean        int
	injected     map[FaultKind]int
	findings     map[FaultKind]int
	unattributed int
	found        []SeriesFinding
	game         string
}

func newSeries(id string, games int, faults Faults) *series {
	return &series{id: id, games: games, faults: faults, status: SeriesPlaying,
		injected: map[FaultKind]int{}, findings: map[FaultKind]int{}, found: []SeriesFinding{}}
}

// tally adds a game that ended won. A finding counts for the fault injected
// at its press, the first if there were several; the faults that caused none
// were handled as they must be.
func (sr *series) tally(v View) {
	at := map[uint64]FaultKind{}
	for _, f := range v.Faults {
		if _, ok := at[f.Rally]; !ok {
			at[f.Rally] = f.Kind
		}
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.played++
	if v.Unexplained == 0 {
		sr.clean++
	} else {
		sr.found = append(sr.found, SeriesFinding{Game: v.ID, Seed: v.Seed, Unexplained: v.Unexplained})
	}
	for _, f := range v.Faults {
		sr.injected[f.Kind]++
	}
	for _, d := range v.Divergences {
		if d.Expected {
			continue
		}
		if kind, ok := at[d.Rally]; ok {
			sr.findings[kind]++
		} else {
			sr.unattributed++
		}
	}
}

func (sr *series) playing(game string) {
	sr.mu.Lock()
	sr.game = game
	sr.mu.Unlock()
}

func (sr *series) end(status SeriesStatus, reason string) {
	sr.mu.Lock()
	sr.status, sr.reason, sr.game = status, reason, ""
	sr.mu.Unlock()
}

func (sr *series) view() SeriesView {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	v := SeriesView{ID: sr.id, Status: sr.status, Reason: sr.reason, Games: sr.games,
		Played: sr.played, Clean: sr.clean, FaultShares: sr.faults, Kinds: []KindTally{},
		Unattributed: sr.unattributed, Findings: append([]SeriesFinding(nil), sr.found...), Game: sr.game}
	// In the order of the fault table, and only the faults the series
	// injects: a fault that is off has nothing to say.
	shares := sr.faults.shares()
	for _, kind := range faultKinds {
		if *shares[kind] == 0 {
			continue
		}
		t := KindTally{Kind: kind, Injected: sr.injected[kind], Findings: sr.findings[kind], Accuracy: 1}
		if t.Injected > 0 {
			t.Accuracy = float64(t.Injected-t.Findings) / float64(t.Injected)
		}
		v.Kinds = append(v.Kinds, t)
	}
	return v
}

// startSeries is POST /series.
func (s *Server) startSeries(w http.ResponseWriter, r *http.Request) {
	var body seriesBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("decoding the body: %w", err))
		return
	}
	if body.Games < 1 || body.Games > maxSeriesGames {
		fail(w, http.StatusBadRequest, fmt.Errorf("games %d: want 1 to %d", body.Games, maxSeriesGames))
		return
	}
	faults := s.cfg.Faults
	if body.Faults != nil {
		if err := body.Faults.Validate(); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		faults = *body.Faults
	}
	id, err := newID()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}

	s.mu.Lock()
	if s.series != nil && s.series.view().Status == SeriesPlaying {
		busy := s.series.id
		s.mu.Unlock()
		fail(w, http.StatusConflict, fmt.Errorf("%w: series %s is still playing", errBusy, busy))
		return
	}
	sr := newSeries(id, body.Games, faults)
	s.series = sr
	s.mu.Unlock()

	// The first game starts here, so a table that is taken is this answer's
	// 409 and not a series that ends at once.
	first, err := s.start(s.seriesGame(faults, body.Seed, 0))
	if err != nil {
		sr.end(SeriesEnded, err.Error())
		status := http.StatusBadGateway
		if errors.Is(err, errBusy) {
			status = http.StatusConflict
		}
		fail(w, status, err)
		return
	}
	sr.playing(first.ID)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.playSeries(sr, first, body.Seed)
	}()
	w.Header().Set("Location", "/series/"+id)
	writeJSON(w, http.StatusCreated, sr.view())
}

// seriesGame is the n-th game of a series: headless, as fast as zaehlwerk
// answers, with the series' faults.
func (s *Server) seriesGame(faults Faults, seed *int64, n int) GameConfig {
	cfg := GameConfig{Faults: faults}
	if seed != nil {
		cfg.Seed, cfg.Seeded = *seed+int64(n), true
	}
	return cfg
}

// playSeries plays a series from its first game, already started, to its
// last.
func (s *Server) playSeries(sr *series, g *Game, seed *int64) {
	for n := 1; ; n++ {
		select {
		case <-g.Done():
		case <-s.ctx.Done():
			sr.end(SeriesEnded, "the service is shutting down")
			return
		}
		v := g.View()
		if v.Status != StatusWon {
			sr.end(SeriesEnded, fmt.Sprintf("game %s %s: %s", v.ID, v.Status, v.Reason))
			return
		}
		sr.tally(v)
		if n == sr.games {
			sr.end(SeriesDone, "")
			return
		}
		next, err := s.start(s.seriesGame(sr.faults, seed, n))
		if err != nil {
			sr.end(SeriesEnded, fmt.Sprintf("game %d of %d did not start: %v", n+1, sr.games, err))
			return
		}
		sr.playing(next.ID)
		g = next
	}
}

// getSeries is GET /series/{id}. The service remembers the last series only.
func (s *Server) getSeries(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	sr := s.series
	s.mu.Unlock()
	if sr == nil || sr.id != r.PathValue("id") {
		fail(w, http.StatusNotFound, fmt.Errorf("no series %q", r.PathValue("id")))
		return
	}
	writeJSON(w, http.StatusOK, sr.view())
}
