package main

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/checkin"
	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
)

// lines is the log, one entry per line.
func lines(logs *bytes.Buffer) []string {
	return strings.Split(strings.TrimSpace(logs.String()), "\n")
}

func TestCheckInIsOffWithOneLinePerPathWithoutSchmetterpause(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	t.Setenv("CHECKIN_TOKEN", "hub-secret")
	t.Setenv("SCHMETTERPAUSE_TOKEN", "sp-secret")

	require.Nil(t, checkinDesk(log, nil))
	got := lines(&logs)
	require.Len(t, got, 2, logs.String())
	require.Contains(t, got[0], "fob check-in disabled")
	require.Contains(t, got[1], "phone check-in disabled")
}

func TestCheckInIsOffWithOneLinePerPathWithoutEitherToken(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	sp, err := schmetterpause.New(schmetterpause.Config{BaseURL: "http://sp.invalid"})
	require.NoError(t, err)

	t.Setenv("CHECKIN_TOKEN", "")
	t.Setenv("SCHMETTERPAUSE_TOKEN", "")
	require.Nil(t, checkinDesk(log, sp))
	got := lines(&logs)
	require.Len(t, got, 2, logs.String())
	require.Contains(t, got[0], "fob check-in disabled, CHECKIN_TOKEN not configured")
	require.Contains(t, got[1], "phone check-in disabled, POST /checkin/claim needs SCHMETTERPAUSE_TOKEN")
}

// ADR-0011: the phone path is Schmetterpause's and needs only its token; the
// hub's CHECKIN_TOKEN turns on the fob path and nothing else.
func TestThePhonePathIsOnWithoutTheHubsToken(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	sp, err := schmetterpause.New(schmetterpause.Config{BaseURL: "http://sp.invalid", Token: "sp-secret"})
	require.NoError(t, err)

	t.Setenv("CHECKIN_TOKEN", "")
	t.Setenv("SCHMETTERPAUSE_TOKEN", "sp-secret")
	require.NotNil(t, checkinDesk(log, sp))
	got := lines(&logs)
	require.Len(t, got, 2, logs.String())
	require.Contains(t, got[0], "fob check-in disabled, CHECKIN_TOKEN not configured")
	require.Contains(t, got[1], "phone check-in enabled")
}

func TestBothPathsSayTheyAreOnInOneLineEach(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	sp, err := schmetterpause.New(schmetterpause.Config{BaseURL: "http://sp.invalid", Token: "sp-secret"})
	require.NoError(t, err)

	t.Setenv("CHECKIN_TOKEN", "hub-secret")
	t.Setenv("SCHMETTERPAUSE_TOKEN", "sp-secret")
	require.NotNil(t, checkinDesk(log, sp))
	got := lines(&logs)
	require.Len(t, got, 2, logs.String())
	require.Contains(t, got[0], "fob check-in enabled")
	require.Contains(t, got[1], "phone check-in enabled")
	require.NotContains(t, logs.String(), "secret", "whether a token is set, never the token")
}

func TestTheCheckInTimeoutIsReadFromTheEnvironment(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sp, err := schmetterpause.New(schmetterpause.Config{BaseURL: "http://sp.invalid"})
	require.NoError(t, err)
	t.Setenv("CHECKIN_TOKEN", "hub-secret")
	t.Setenv("SCHMETTERPAUSE_TOKEN", "")

	t.Setenv("CHECKIN_TIMEOUT", "")
	require.Equal(t, checkin.DefaultTimeout, checkinDesk(log, sp).Timeout())

	t.Setenv("CHECKIN_TIMEOUT", "90s")
	require.Equal(t, 90*time.Second, checkinDesk(log, sp).Timeout())

	t.Setenv("CHECKIN_TIMEOUT", "soon")
	require.Equal(t, checkin.DefaultTimeout, checkinDesk(log, sp).Timeout(), "ignored with a warning")
}
