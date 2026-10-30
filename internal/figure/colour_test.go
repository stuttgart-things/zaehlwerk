package figure

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Every pair of Schmetterpause's blades, resolved as the panel
// (homerun2-led-catcher's panel_colors) resolves it: zaehlwerk sends the
// panel colours it already maps onto itself, and the game's paddles have the
// colours the names have on the matrix.
func TestEveryPairOfBladesResolvesAsThePanelResolvesIt(t *testing.T) {
	for _, tc := range []struct{ a, b, wantA, wantB string }{
		{"#3aa6ff", "#3aa6ff", "#2896ff", "#ff00aa"},
		{"#3aa6ff", "#2f6fd0", "#2896ff", "#ff00aa"},
		{"#3aa6ff", "#159c8c", "#2896ff", "#00e6a0"},
		{"#3aa6ff", "#4d7c4a", "#2896ff", "#28dc28"},
		{"#3aa6ff", "#d64f9c", "#2896ff", "#ff00aa"},
		{"#3aa6ff", "#c05fd0", "#2896ff", "#963cff"},
		{"#3aa6ff", "#8c8f9c", "#2896ff", "#ffc800"},
		{"#2f6fd0", "#3aa6ff", "#2896ff", "#ff00aa"},
		{"#2f6fd0", "#2f6fd0", "#2896ff", "#ff00aa"},
		{"#2f6fd0", "#159c8c", "#2896ff", "#00e6a0"},
		{"#2f6fd0", "#4d7c4a", "#2896ff", "#28dc28"},
		{"#2f6fd0", "#d64f9c", "#2896ff", "#ff00aa"},
		{"#2f6fd0", "#c05fd0", "#2896ff", "#963cff"},
		{"#2f6fd0", "#8c8f9c", "#2896ff", "#ffc800"},
		{"#159c8c", "#3aa6ff", "#00e6a0", "#2896ff"},
		{"#159c8c", "#2f6fd0", "#00e6a0", "#2896ff"},
		{"#159c8c", "#159c8c", "#00e6a0", "#963cff"},
		{"#159c8c", "#4d7c4a", "#00e6a0", "#28dc28"},
		{"#159c8c", "#d64f9c", "#00e6a0", "#ff00aa"},
		{"#159c8c", "#c05fd0", "#00e6a0", "#963cff"},
		{"#159c8c", "#8c8f9c", "#00e6a0", "#ffc800"},
		{"#4d7c4a", "#3aa6ff", "#28dc28", "#2896ff"},
		{"#4d7c4a", "#2f6fd0", "#28dc28", "#2896ff"},
		{"#4d7c4a", "#159c8c", "#28dc28", "#00e6a0"},
		{"#4d7c4a", "#4d7c4a", "#28dc28", "#ff6400"},
		{"#4d7c4a", "#d64f9c", "#28dc28", "#ff00aa"},
		{"#4d7c4a", "#c05fd0", "#28dc28", "#963cff"},
		{"#4d7c4a", "#8c8f9c", "#28dc28", "#ffc800"},
		{"#d64f9c", "#3aa6ff", "#ff00aa", "#2896ff"},
		{"#d64f9c", "#2f6fd0", "#ff00aa", "#2896ff"},
		{"#d64f9c", "#159c8c", "#ff00aa", "#00e6a0"},
		{"#d64f9c", "#4d7c4a", "#ff00aa", "#28dc28"},
		{"#d64f9c", "#d64f9c", "#ff00aa", "#2896ff"},
		{"#d64f9c", "#c05fd0", "#ff00aa", "#963cff"},
		{"#d64f9c", "#8c8f9c", "#ff00aa", "#ffc800"},
		{"#c05fd0", "#3aa6ff", "#963cff", "#2896ff"},
		{"#c05fd0", "#2f6fd0", "#963cff", "#2896ff"},
		{"#c05fd0", "#159c8c", "#963cff", "#00e6a0"},
		{"#c05fd0", "#4d7c4a", "#963cff", "#28dc28"},
		{"#c05fd0", "#d64f9c", "#963cff", "#ff00aa"},
		{"#c05fd0", "#c05fd0", "#963cff", "#ff6400"},
		{"#c05fd0", "#8c8f9c", "#963cff", "#ffc800"},
		{"#8c8f9c", "#3aa6ff", "#ffc800", "#2896ff"},
		{"#8c8f9c", "#2f6fd0", "#ffc800", "#2896ff"},
		{"#8c8f9c", "#159c8c", "#ffc800", "#00e6a0"},
		{"#8c8f9c", "#4d7c4a", "#ffc800", "#28dc28"},
		{"#8c8f9c", "#d64f9c", "#ffc800", "#ff00aa"},
		{"#8c8f9c", "#c05fd0", "#ffc800", "#963cff"},
		{"#8c8f9c", "#8c8f9c", "#ffc800", "#2896ff"},
	} {
		require.Equal(t, [2]string{tc.wantA, tc.wantB}, Resolve(tc.a, tc.b), "%s against %s", tc.a, tc.b)
	}
}

// Cleo's light blue and Gero's blue were blue and cyan on the panel: told
// apart only by how bright they were.
func TestTwoSidesOnOneColourAreToldApartByHue(t *testing.T) {
	for _, pair := range [][2]string{{"#3aa6ff", "#2f6fd0"}, {"#8c8f9c", "#8c8f9c"}, {"#4d7c4a", "#4d7c4a"}, {"", ""}} {
		got := Resolve(pair[0], pair[1])
		require.GreaterOrEqual(t, hueDistance(hue(got[0]), hue(got[1])), minHueApart, pair)
	}
}

func TestAGreyBladeIsYellow(t *testing.T) {
	require.Equal(t, "#ffc800", Resolve("#8c8f9c", "#159c8c")[0])
	require.Equal(t, "#ffc800", Resolve("", "#159c8c")[0], "no blade counts as a grey")
	require.Equal(t, "#ffc800", Resolve("not a colour", "#159c8c")[0])
}

// The panel maps what it is sent once more; a resolved pair has to come back
// as it went.
func TestAResolvedPairStaysAsItIs(t *testing.T) {
	for _, a := range Panel {
		for _, b := range Panel {
			if a == b {
				continue
			}
			require.Equal(t, [2]string{a, b}, Resolve(a, b))
		}
	}
}
