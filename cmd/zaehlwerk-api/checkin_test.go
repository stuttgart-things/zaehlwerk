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

func TestCheckInIsOffWithOneLineWithoutSchmetterpause(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))

	require.Nil(t, checkinDesk(log, nil))
	require.Equal(t, 1, strings.Count(logs.String(), "\n"), logs.String())
	require.Contains(t, logs.String(), "check-in disabled")
}

func TestCheckInIsOffWithOneLineWithoutAToken(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	sp, err := schmetterpause.New(schmetterpause.Config{BaseURL: "http://sp.invalid"})
	require.NoError(t, err)

	t.Setenv("CHECKIN_TOKEN", "")
	require.Nil(t, checkinDesk(log, sp))
	require.Equal(t, 1, strings.Count(logs.String(), "\n"), logs.String())
	require.Contains(t, logs.String(), "check-in disabled, CHECKIN_TOKEN not configured")
}

func TestTheCheckInTimeoutIsReadFromTheEnvironment(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sp, err := schmetterpause.New(schmetterpause.Config{BaseURL: "http://sp.invalid"})
	require.NoError(t, err)
	t.Setenv("CHECKIN_TOKEN", "hub-secret")

	t.Setenv("CHECKIN_TIMEOUT", "")
	require.Equal(t, checkin.DefaultTimeout, checkinDesk(log, sp).Timeout())

	t.Setenv("CHECKIN_TIMEOUT", "90s")
	require.Equal(t, 90*time.Second, checkinDesk(log, sp).Timeout())

	t.Setenv("CHECKIN_TIMEOUT", "soon")
	require.Equal(t, checkin.DefaultTimeout, checkinDesk(log, sp).Timeout(), "ignored with a warning")
}
