package pong

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTheCourtRunsFasterByTheGivenFactorAndNoFasterThanTenTimes(t *testing.T) {
	require.Equal(t, servedTick, courtTickFor(0), "unset is real time")
	require.Equal(t, servedTick, courtTickFor(1))
	require.Equal(t, servedTick/4, courtTickFor(4))
	require.Equal(t, courtTickFor(MaxCourtSpeed), courtTickFor(50))
	require.Less(t, courtTickFor(MaxCourtSpeed), courtTickFor(4))
}
