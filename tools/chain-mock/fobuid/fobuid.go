// Package fobuid gives every player of the chain mock a key fob of their own.
//
// The table mock offers one fob per player on its page, and the fake
// Schmetterpause has those same fobs on its roster, so the two agree without
// a list in two places. A real Schmetterpause keeps only an HMAC of a UID
// (its ADR-0025) and never says whose a fob is: against it, the fob for a
// player is a fob nobody owns until that player links it.
package fobuid

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// For is the UID of playerID's fob: seven bytes, as an NTAG has, starting
// with NXP's manufacturer byte 0x04. Written the one way
// checkin.NormaliseUID writes a UID, upper case with colons.
//
// Derived, not random, so the same player gets the same fob after a restart
// of either mock, and a fob linked once stays linked.
func For(playerID string) string {
	sum := sha256.Sum256([]byte(playerID))
	parts := []string{"04"}
	for _, b := range sum[:6] {
		parts = append(parts, fmt.Sprintf("%02X", b))
	}
	return strings.Join(parts, ":")
}
