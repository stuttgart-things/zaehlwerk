package main

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

// The office: Schmetterpause is there and MATCHES_TOKEN is not, so POST
// /matches takes no ids and the log says what is missing (ADR-0015).
func TestTheHandoverDoorIsOffWithoutTheMatchesToken(t *testing.T) {
	var logs bytes.Buffer
	t.Setenv("MATCHES_TOKEN", "")

	require.Empty(t, handoverToken(slog.New(slog.NewTextHandler(&logs, nil)), true))
	got := lines(&logs)
	require.Len(t, got, 1, logs.String())
	require.Contains(t, got[0], "match handover disabled, POST /matches with player ids needs MATCHES_TOKEN")
}

// A token with nobody to report to would take a handover that goes nowhere.
func TestTheHandoverDoorIsOffWithoutSchmetterpauseWhateverTheToken(t *testing.T) {
	var logs bytes.Buffer
	t.Setenv("MATCHES_TOKEN", "matches-secret")

	require.Empty(t, handoverToken(slog.New(slog.NewTextHandler(&logs, nil)), false))
	got := lines(&logs)
	require.Len(t, got, 1, logs.String())
	require.Contains(t, got[0], "needs SCHMETTERPAUSE_URL")
}

func TestTheHandoverDoorIsOnWithTheTokenAndSchmetterpause(t *testing.T) {
	var logs bytes.Buffer
	t.Setenv("MATCHES_TOKEN", "matches-secret")

	require.Equal(t, "matches-secret", handoverToken(slog.New(slog.NewTextHandler(&logs, nil)), true))
	require.Contains(t, logs.String(), "match handover enabled")
	require.NotContains(t, logs.String(), "matches-secret", "the token is never logged")
}
