package figure

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/face"
)

const drawing = `<svg viewBox="0 0 420 420" data-face="neutral" style="--fur:#f89828;--paddle:#1E88E5"></svg>`

// source counts who was asked for.
type source struct {
	mu  sync.Mutex
	ids []string
}

func (s *source) Figure(_ context.Context, id string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids = append(s.ids, id)
	return []byte(drawing), nil
}

func (s *source) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ids...)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// A random figure is a new one every time it is asked for, so a match fetches
// once, however many screens render it.
func TestAMatchFetchesItsFiguresOnce(t *testing.T) {
	src := &source{}
	c := NewCache(src, quiet())
	ids := func(context.Context) [2]string { return [2]string{"anna", ""} }

	first := c.Ensure("m1", ids)
	require.Same(t, first, c.Ensure("m1", ids))
	require.Eventually(t, func() bool { return c.Ready("m1") != nil }, time.Second, time.Millisecond)

	require.Equal(t, []string{"anna", ""}, src.asked())
	require.Equal(t, "#1e88e5", first.Side[0].Blade)
}

func TestAMatchNobodyAskedAboutIsNotReady(t *testing.T) {
	require.Nil(t, NewCache(&source{}, quiet()).Ready("m1"))
}

func TestTheCacheKeepsTheLastMatchesOnly(t *testing.T) {
	src := &source{}
	c := NewCache(src, quiet())
	none := func(context.Context) [2]string { return [2]string{} }
	for i := range keep + 1 {
		<-c.Ensure(string(rune('a'+i)), none).Done
	}

	<-c.Ensure("a", none).Done

	require.Len(t, src.asked(), 2*(keep+2), "the first match fell out and fetched again")
}

func TestAFaceIsSetOnTheFiguresRoot(t *testing.T) {
	require.Contains(t, string(WithFace([]byte(drawing), face.Happy)), `<svg viewBox="0 0 420 420" data-face="happy"`)
	require.Equal(t, `<svg data-face="sad"></svg>`, string(WithFace([]byte(`<svg></svg>`), face.Sad)))
}

func TestOnlyAKnownFaceIsAsked(t *testing.T) {
	require.Equal(t, face.Angry, Asked("angry"))
	require.Equal(t, face.Neutral, Asked(`"><script>`))
	require.Equal(t, face.Neutral, Asked(""))
}

func TestAFigureWithoutABladeSaysNone(t *testing.T) {
	require.Empty(t, BladeOf([]byte(`<svg></svg>`)))
}
