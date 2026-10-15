package fobuid

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/checkin"
)

func TestAFobIsAUIDZaehlwerkTakesAsWritten(t *testing.T) {
	uid := For("11111111-1111-4111-8111-111111111111")
	normal, err := checkin.NormaliseUID(uid)
	require.NoError(t, err)
	require.Equal(t, uid, normal, "written the one way the desk writes it")
	require.Len(t, uid, len("04:00:00:00:00:00:00"))
}

func TestEveryPlayerHasTheirOwnFobAndKeepsIt(t *testing.T) {
	a, b := For("11111111-1111-4111-8111-111111111111"), For("22222222-2222-4222-8222-222222222222")
	require.NotEqual(t, a, b)
	require.Equal(t, a, For("11111111-1111-4111-8111-111111111111"))
}
