package qr_test

import (
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	upstream "rsc.io/qr"

	"github.com/stuttgart-things/zaehlwerk/internal/qr"
)

const sample = "https://schmetterpause.example.com/table/left"

// rect matches one "M<x> <y>h<n>v1h-<n>z" run.
var rect = regexp.MustCompile(`M(\d+) (\d+)h(\d+)v1h-(\d+)z`)

// cells parses the rendered path back into the black modules it draws, and
// fails on anything in the path that is not a run: the page embeds the string
// as it is, so a leftover would be drawn and never noticed.
func cells(t *testing.T, path string) map[[2]int]bool {
	t.Helper()

	black := make(map[[2]int]bool)
	end := 0
	for _, m := range rect.FindAllStringSubmatchIndex(path, -1) {
		require.Equal(t, end, m[0], "path holds %q between two runs", path[end:m[0]])
		end = m[1]

		x, _ := strconv.Atoi(path[m[2]:m[3]])
		y, _ := strconv.Atoi(path[m[4]:m[5]])
		run, _ := strconv.Atoi(path[m[6]:m[7]])
		require.Equal(t, strconv.Itoa(run), path[m[8]:m[9]], "a run that does not close")
		for i := range run {
			require.False(t, black[[2]int{x + i, y}], "module %d,%d drawn twice", x+i, y)
			black[[2]int{x + i, y}] = true
		}
	}
	require.Equal(t, len(path), end, "trailing %q", path[end:])
	return black
}

// The point of the package: whatever the encoder considers black ends up in
// the path, shifted by the quiet zone and by nothing else. A transposed or
// off-by-one grid still looks like a QR code and simply does not scan.
func TestThePathDrawsExactlyTheEncodedModules(t *testing.T) {
	code, err := qr.Render(sample)
	require.NoError(t, err)

	want, err := upstream.Encode(sample, upstream.M)
	require.NoError(t, err)
	require.Equal(t, want.Size+2*qr.QuietZone, code.Size)

	black := cells(t, code.Path)
	for y := range want.Size {
		for x := range want.Size {
			require.Equal(t, want.Black(x, y), black[[2]int{x + qr.QuietZone, y + qr.QuietZone}],
				"module %d,%d", x, y)
		}
	}
	require.Len(t, black, func() int {
		n := 0
		for y := range want.Size {
			for x := range want.Size {
				if want.Black(x, y) {
					n++
				}
			}
		}
		return n
	}(), "nothing drawn in the quiet zone")
}

func TestATextTooLongForACodeIsAnError(t *testing.T) {
	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'a'
	}
	_, err := qr.Render(string(long))
	require.Error(t, err)
}
