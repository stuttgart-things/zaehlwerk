package main

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/schmetterpause"
)

func TestTheReportDelayIsReadFromTheEnvironment(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Setenv("SCHMETTERPAUSE_REPORT_DELAY", "")
	d, err := reportDelay(log, true)
	require.NoError(t, err)
	require.Equal(t, schmetterpause.DefaultDelay, d)

	t.Setenv("SCHMETTERPAUSE_REPORT_DELAY", "2s")
	d, err = reportDelay(log, true)
	require.NoError(t, err)
	require.Equal(t, 2*time.Second, d)

	t.Setenv("SCHMETTERPAUSE_REPORT_DELAY", "0")
	d, err = reportDelay(log, true)
	require.NoError(t, err)
	require.Zero(t, d, "0 reports at once")
}

func TestAReportDelayThatIsNoDurationStopsTheStart(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, raw := range []string{"15", "soon", "-1s"} {
		t.Setenv("SCHMETTERPAUSE_REPORT_DELAY", raw)
		_, err := reportDelay(log, true)
		require.Error(t, err, raw)
		require.Contains(t, err.Error(), "SCHMETTERPAUSE_REPORT_DELAY")
	}
}
