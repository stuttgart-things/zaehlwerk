package pong

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// The waits of a check are short here, as the attract mode's are: the rules do
// not change with the length of a prompt.
const (
	testCheckTimeout = 300 * time.Millisecond
	testCheckGap     = 80 * time.Millisecond
)

// buttons are the two buttons at the table as the tests press them: each a
// source of its own with its own counter, posting to /ingest/button with its
// half, the way the hardware does. Nothing goes through the game.
type buttons struct {
	t   *testing.T
	url string
	n   map[scorer.Half]uint64
}

func newButtons(t *testing.T, zaehlwerkURL string) *buttons {
	return &buttons{t: t, url: zaehlwerkURL, n: map[scorer.Half]uint64{}}
}

func (b *buttons) press(h scorer.Half) {
	b.t.Helper()
	b.n[h]++
	body := fmt.Sprintf(`{"events": [{"source": "button-%s", "half": "%s", "event_id": %d}]}`, h, h, b.n[h])
	resp, err := http.Post(b.url+"/ingest/button", "application/json", strings.NewReader(body))
	require.NoError(b.t, err)
	_ = resp.Body.Close()
	require.Equal(b.t, http.StatusOK, resp.StatusCode)
}

// newCheck starts a button check with short waits and returns its id.
func newCheck(t *testing.T, p *pongFixture) string {
	t.Helper()
	p.pong.cfg.CheckTimeout = testCheckTimeout
	p.pong.checkGap = testCheckGap
	status, v, raw := p.startWith(startBody{Driver: DriverCheck})
	require.Equal(t, http.StatusCreated, status, raw)
	require.Equal(t, DriverCheck, v.Driver)
	require.Equal(t, CheckNames, v.Players)
	return v.ID
}

// prompt waits for the check's next prompt, the one after `after`, and returns
// it, or nil once the check is over.
func (f *pongFixture) prompt(id string, after int) *Prompt {
	f.t.Helper()
	var p *Prompt
	require.Eventually(f.t, func() bool {
		v := f.get(id)
		if v.Status != StatusPlaying {
			p = nil
			return true
		}
		p = v.Check.Prompt
		return p != nil && p.N > after
	}, 5*time.Second, time.Millisecond)
	return p
}

// follow presses what the page says until the check is over, with each press
// passed through press first so a test can press something else.
func (f *pongFixture) followPrompts(id string, b *buttons, press func(*Prompt) bool) []Prompt {
	f.t.Helper()
	var seen []Prompt
	n := 0
	for {
		p := f.prompt(id, n)
		if p == nil {
			return seen
		}
		seen = append(seen, *p)
		n = p.N
		if press == nil || press(p) {
			b.press(p.Half)
		}
		require.Less(f.t, n, maxCheckPrompts)
	}
}

func TestAButtonCheckPressedAsToldHasNoFinding(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)
	id := newCheck(t, p)

	prompts := p.followPrompts(id, newButtons(t, tbl.url), nil)
	v := p.finished(id)

	require.Equal(t, StatusChecked, v.Status, v.Reason)
	require.Empty(t, v.Check.Findings)
	require.Equal(t, len(prompts), v.Check.Counts[CheckOK])
	require.Equal(t, len(prompts), v.Check.Prompts)
	require.Contains(t, v.Check.Summary, "no findings")

	// What a check sets out to see: both ready presses, the rally for the
	// serve, points on both halves, a change of ends, and after it both
	// halves again, now each for the other player.
	count := func(ph scorer.Phase, kind PressKind) int {
		n := 0
		for _, p := range prompts {
			if p.Phase == ph && p.Kind == kind {
				n++
			}
		}
		return n
	}
	require.Equal(t, 2, count(scorer.PhaseWarmup, PressReady))
	require.Equal(t, 1, count(scorer.PhaseServeRally, PressRally))
	require.Equal(t, 2, count(scorer.PhaseChangeEnds, PressReady))

	before := map[scorer.Half]string{}
	after := map[scorer.Half]string{}
	changed := false
	for _, p := range prompts {
		switch {
		case p.Phase == scorer.PhaseChangeEnds:
			changed = true
		case p.Phase == scorer.PhasePlaying && !changed:
			before[p.Half] = p.For
		case p.Phase == scorer.PhasePlaying:
			after[p.Half] = p.For
		}
	}
	require.Len(t, before, 2, "points on both halves before the change of ends")
	require.Len(t, after, 2, "and both halves after it")
	for _, h := range []scorer.Half{scorer.HalfLeft, scorer.HalfRight} {
		require.NotEqual(t, before[h], after[h], "the same button counts for the other player after the change")
	}

	// The check is over, and so is its match: the table is free again.
	m, err := tbl.registry.Get(v.MatchID)
	require.NoError(t, err)
	require.False(t, m.Running())
	require.False(t, m.Handover.Wanted(), "a check is never reported")
}

func TestAButtonCheckIsTheSameWithTheSameSeed(t *testing.T) {
	run := func() []string {
		tbl := newTable(t)
		p := newPong(t, tbl.url, 7)
		var asked []string
		for _, pr := range p.followPrompts(newCheck(t, p), newButtons(t, tbl.url), nil) {
			asked = append(asked, string(pr.Phase)+" "+string(pr.Half))
		}
		return asked
	}
	require.Equal(t, run(), run())
}

func TestAPressAtTheWrongHalfIsAFinding(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)
	id := newCheck(t, p)
	b := newButtons(t, tbl.url)

	wrong := 0
	p.followPrompts(id, b, func(pr *Prompt) bool {
		// The rally for the serve, pressed at the other half.
		if pr.Phase == scorer.PhaseServeRally && wrong == 0 {
			wrong = pr.N
			b.press(other(pr.Half))
			return false
		}
		return true
	})
	v := p.finished(id)

	require.Equal(t, StatusChecked, v.Status, v.Reason)
	require.Equal(t, 1, v.Check.Counts[CheckWrongHalf])
	require.Len(t, v.Check.Findings, 1)
	f := v.Check.Findings[0]
	require.Equal(t, CheckWrongHalf, f.Outcome)
	require.Equal(t, wrong, f.Prompt)
	require.Contains(t, f.Got, "serve_decided")
	require.Contains(t, v.Check.Summary, "wrong half")
}

func TestASecondPressRightAfterTheFirstIsABounce(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)
	id := newCheck(t, p)
	b := newButtons(t, tbl.url)

	bounced := 0
	p.followPrompts(id, b, func(pr *Prompt) bool {
		if pr.Phase == scorer.PhasePlaying && bounced == 0 {
			bounced = pr.N
			b.press(pr.Half)
			b.press(pr.Half) // the button bounces: a second press, a new event id
			return false
		}
		return true
	})
	v := p.finished(id)

	require.Equal(t, StatusChecked, v.Status, v.Reason)
	require.Equal(t, 1, v.Check.Counts[CheckDouble], "%+v", v.Check.Findings)
	require.Len(t, v.Check.Findings, 1)
	require.Equal(t, CheckDouble, v.Check.Findings[0].Outcome)
	require.Equal(t, bounced, v.Check.Findings[0].Prompt)
}

func TestAPromptNobodyPressesForIsMissing(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)
	id := newCheck(t, p)
	b := newButtons(t, tbl.url)

	skipped := 0
	p.followPrompts(id, b, func(pr *Prompt) bool {
		if pr.Phase == scorer.PhasePlaying && skipped == 0 {
			skipped = pr.N
			return false
		}
		return true
	})
	v := p.finished(id)

	require.Equal(t, StatusChecked, v.Status, v.Reason)
	require.Equal(t, 1, v.Check.Counts[CheckMissing])
	require.Equal(t, CheckMissing, v.Check.Findings[0].Outcome)
	require.Equal(t, skipped, v.Check.Findings[0].Prompt)
	require.Equal(t, "nothing arrived", v.Check.Findings[0].Got)
}

func TestAMissingReadySaysItMayBeAWrongHalf(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)
	id := newCheck(t, p)
	b := newButtons(t, tbl.url)

	skipped := 0
	p.followPrompts(id, b, func(pr *Prompt) bool {
		if pr.Kind == PressReady && skipped == 0 {
			skipped = pr.N
			return false
		}
		return true
	})
	v := p.finished(id)

	require.Equal(t, StatusChecked, v.Status, v.Reason)
	require.Len(t, v.Check.Findings, 1)
	require.Equal(t, CheckMissing, v.Check.Findings[0].Outcome)
	require.Equal(t, skipped, v.Check.Findings[0].Prompt)
	require.Equal(t, "nothing arrived; may also be a press at the wrong half, which zaehlwerk ignores here", v.Check.Findings[0].Got)
}

func TestAPressBetweenTwoPromptsIsUnexpected(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)
	id := newCheck(t, p)
	b := newButtons(t, tbl.url)

	done := false
	p.followPrompts(id, b, func(pr *Prompt) bool {
		if pr.Phase == scorer.PhasePlaying && !done {
			done = true
			b.press(pr.Half)
			// Right after the answer, in the gap: the other button, which
			// nobody asked for.
			b.press(other(pr.Half))
			return false
		}
		return true
	})
	v := p.finished(id)

	require.Equal(t, StatusChecked, v.Status, v.Reason)
	require.Equal(t, 1, v.Check.Counts[CheckUnexpected], "%+v", v.Check.Findings)
	require.Equal(t, CheckUnexpected, v.Check.Findings[0].Outcome)
	require.Zero(t, v.Check.Findings[0].Prompt, "it belongs to no prompt")
}

func TestAButtonCheckNobodyPressesInEndsItsMatch(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)
	id := newCheck(t, p)

	v := p.finished(id)
	require.Equal(t, StatusEnded, v.Status)
	require.Equal(t, errNobodyPresses.Error(), v.Reason)
	require.Equal(t, maxMissed, v.Check.Counts[CheckMissing])
	m, err := tbl.registry.Get(v.MatchID)
	require.NoError(t, err)
	require.False(t, m.Running())
}

func TestAButtonCheckSendsNothingItself(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)
	p.pong.cfg.CheckTimeout = time.Minute
	b := startBrowser(t, p, map[string]any{"driver": DriverCheck})
	first := p.prompt(b.id, 0)

	// Nobody pressed, so nothing moved: the check asks and waits.
	time.Sleep(4 * testCheckGap)
	v := p.get(b.id)
	require.Equal(t, scorer.PhaseWarmup, v.State.Phase)
	require.Equal(t, first.N, v.Check.Prompt.N)
	require.Len(t, v.State.Waiting, 2)

	// Its token ends it, and presses nothing: the press endpoint is not a
	// way in for a check.
	status, answer := b.pressWith(b.token, Press{Rally: 1, Kind: PressReady, Half: first.Half})
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, answer.Error, "a button check takes no presses")
	require.Equal(t, scorer.PhaseWarmup, p.get(b.id).State.Phase)
}

func TestAButtonCheckTakesTheTableFromADemo(t *testing.T) {
	tbl := newTable(t)
	p, _ := newAttract(t, tbl.url, nil)
	demo := p.demo()

	id := newCheck(t, p)
	require.Equal(t, errYielded.Error(), p.get(demo.ID).Reason)
	cur, err := tbl.registry.Current()
	require.NoError(t, err)
	require.Equal(t, p.get(id).MatchID, cur.ID)
}

func TestAButtonCheckTakesNoFaults(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)
	status, _, raw := p.startWith(map[string]any{"driver": DriverCheck, "faults": Faults{Lost: 1}})
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, raw, "a button check takes no")
}

func TestTheLobbyStartsAButtonCheckWithoutNamesOrFaults(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)
	p.pong.cfg.CheckTimeout = time.Minute
	form := url.Values{"mode": {ModeCheck}, "name_a": {"Anna"}, "fault_lost": {"1"}}
	req, err := http.NewRequest(http.MethodPost, p.url+"/ui/games", strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	target, token, ok := strings.Cut(resp.Header.Get("HX-Redirect"), "#")
	require.True(t, ok)
	require.NotEmpty(t, token, "the token that ends it")
	id := strings.TrimPrefix(target, "/play/")
	v := p.get(id)
	require.Equal(t, DriverCheck, v.Driver)
	require.Equal(t, CheckNames, v.Players)
	require.Equal(t, Faults{}, v.FaultShares)

	first := p.prompt(id, 0)
	page := mustGet(t, p.url+target)
	require.Contains(t, page, "Bereit: "+halfLabels[first.Half])
	require.Contains(t, page, "zählt für "+first.For)
	require.Contains(t, page, `src="/static/check.js"`)
	require.NotContains(t, page, "<canvas", "a check has no game to draw")
	require.NotContains(t, page, "Wahrheit gegen Zählwerk")

	require.Contains(t, mustGet(t, p.url+"/"), "als Tasterprüfung")
}

func TestTheCheckPageEndsWithTheSummaryToCopy(t *testing.T) {
	tbl := newTable(t)
	p := newPong(t, tbl.url, 1)
	id := newCheck(t, p)
	b := newButtons(t, tbl.url)
	wrong := false
	p.followPrompts(id, b, func(pr *Prompt) bool {
		if pr.Phase == scorer.PhasePlaying && !wrong {
			wrong = true
			b.press(other(pr.Half))
			return false
		}
		return true
	})
	v := p.finished(id)
	require.Equal(t, StatusChecked, v.Status)

	page := mustGet(t, p.url+"/play/"+id)
	require.Contains(t, page, "TASTER GEPRÜFT")
	require.Contains(t, page, "falsche Hälfte")
	require.Contains(t, page, `id="summary"`)
	require.Contains(t, page, "zaehlwerk-pong button check "+id)
	require.NotContains(t, page, "drücken</div>", "no prompt once it is over")
}

func TestTheCheckScriptIsServedAndNothingElseIs(t *testing.T) {
	p := newPong(t, newTable(t).url, 1)
	require.Contains(t, mustGet(t, p.url+"/static/check.js"), "pong-token-")
	resp, err := http.Get(p.url + "/static/lobby.html")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}
