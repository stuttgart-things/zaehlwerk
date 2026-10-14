package checkin

import (
	"encoding/hex"
	"errors"
	"strings"
)

// ErrBadUID is a UID that is not what a reader produces. Deliberately without
// the value: the value is a card number (ADR-0010).
var ErrBadUID = errors.New("uid: must be 4, 7 or 10 bytes of hex, with ':', '-' or ' ' between them or nothing")

// NormaliseUID checks a UID the way Schmetterpause does and writes it in one
// spelling, upper case with colons: 04:A3:1B:22.
//
// Checked here as well as there so that a malformed UID is refused at the
// door, without a call to Schmetterpause, and the answer to the hub can say
// why without quoting it.
func NormaliseUID(uid string) (string, error) {
	clean := strings.Map(func(r rune) rune {
		switch r {
		case ':', '-', ' ':
			return -1
		}
		return r
	}, uid)
	raw, err := hex.DecodeString(clean)
	if err != nil {
		return "", ErrBadUID
	}
	switch len(raw) {
	case 4, 7, 10:
	default:
		return "", ErrBadUID
	}

	parts := make([]string, len(raw))
	for i, b := range raw {
		parts[i] = strings.ToUpper(hex.EncodeToString([]byte{b}))
	}
	return strings.Join(parts, ":"), nil
}
