package figure

import (
	"math"
	"strconv"
)

// The colours the players have on the panel and on the game's board. A
// figure's blade is picked for a screen in Schmetterpause: a grey one is a
// fine choice there and, on the panel's LEDs, dimmed white — close to the
// unlit pixels and to the white serve marker. And two players' blades can be
// the same, or two blues that only differ in how bright they are, which a few
// metres from the panel is not at all.
//
// So each side's blade is mapped to the nearest of Panel by hue, a grey to the
// yellow, which no blade is. When b lands on a's colour, b takes the nearest
// of those at least minHueApart from a's: light blue against blue is blue
// against magenta, not blue against cyan. Panel's blue is a light one: a deep
// blue is the dimmest colour on the LEDs.
//
// The panel (homerun2-led-catcher, PANEL_COLORS in display/score.py) maps the
// colours it is sent onto the same list the same way, so what this sends it
// stays as it is. The two lists have to stay the same.

// Panel is the colours a side may have.
var Panel = []string{
	"#2896ff", // blue
	"#00c8ff", // cyan
	"#00e6a0", // teal
	"#28dc28", // green
	"#ffc800", // yellow
	"#ff6400", // orange
	"#ff1e32", // red
	"#ff00aa", // magenta
	"#963cff", // violet
}

const (
	// greyColour is Panel's yellow, where a colour without a hue goes.
	greyColour = 4
	// minSaturation is the HSV saturation below which a colour is a grey.
	minSaturation = 0.25
	// minHueApart is how far apart in hue, as a share of the wheel, two
	// sides are when they had to be told apart.
	minHueApart = 0.25
)

// Resolve is the colours of a match's two sides, a first, from the blades
// their figures carry: never the same, and two that had to be told apart a
// clear hue apart. A blade that is not #rrggbb, or empty, counts as a grey.
func Resolve(a, b string) [2]string {
	first := ranked(a)[0]
	second := ranked(b)[0]
	if second == first {
		apart := hue(Panel[first])
		for _, i := range ranked(b) {
			if hueDistance(hue(Panel[i]), apart) >= minHueApart {
				second = i
				break
			}
		}
	}
	return [2]string{Panel[first], Panel[second]}
}

// ranked is Panel's indices, nearest to c first.
func ranked(c string) []int {
	h, s := hueSat(c)
	order := make([]int, 0, len(Panel))
	if s < minSaturation {
		order = append(order, greyColour)
		for i := range Panel {
			if i != greyColour {
				order = append(order, i)
			}
		}
		return order
	}
	for i := range Panel {
		order = append(order, i)
	}
	// Stable, so a tie goes to the earlier colour, as Python's sorted does.
	for i := 1; i < len(order); i++ {
		for j := i; j > 0 && hueDistance(h, hue(Panel[order[j]])) < hueDistance(h, hue(Panel[order[j-1]])); j-- {
			order[j], order[j-1] = order[j-1], order[j]
		}
	}
	return order
}

func hue(c string) float64 {
	h, _ := hueSat(c)
	return h
}

func hueDistance(a, b float64) float64 {
	d := math.Abs(a - b)
	return math.Min(d, 1-d)
}

// hueSat is c's HSV hue, as a share of the wheel, and saturation. Anything
// but #rrggbb is a grey.
func hueSat(c string) (float64, float64) {
	if len(c) != 7 || c[0] != '#' {
		return 0, 0
	}
	v, err := strconv.ParseUint(c[1:], 16, 32)
	if err != nil {
		return 0, 0
	}
	r, g, b := float64(v>>16&0xff)/255, float64(v>>8&0xff)/255, float64(v&0xff)/255
	hi, lo := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	if hi == 0 {
		return 0, 0
	}
	d := hi - lo
	s := d / hi
	if d == 0 {
		return 0, s
	}
	var h float64
	switch hi {
	case r:
		h = math.Mod((g-b)/d, 6)
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	h /= 6
	if h < 0 {
		h++
	}
	return h, s
}
