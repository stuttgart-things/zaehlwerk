package checkin

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/stuttgart-things/zaehlwerk/internal/scorer"
)

// maxPhoneClaims bounds the check-in ids a repeated claim is recognised by.
// One table sees a handful of claims in the minutes one lasts; this is a
// bound on what the caller can make the process remember, as maxSources is
// for the hub.
const maxPhoneClaims = 256

// PhoneClaim is one claim from Schmetterpause for a player who checked in
// with their phone (ADR-0011). Schmetterpause recorded the check-in already,
// so it brings what a fob resolve would return.
type PhoneClaim struct {
	Half      scorer.Half
	PlayerID  string
	CheckinID string
}

// Validate checks the claim and writes both ids in their one spelling, so
// that a repeat is recognised however the caller cased it.
func (c *PhoneClaim) Validate() error {
	if c.Half != scorer.HalfLeft && c.Half != scorer.HalfRight {
		return fmt.Errorf("half: must be %q or %q", scorer.HalfLeft, scorer.HalfRight)
	}
	player, err := canonicalUUID(c.PlayerID)
	if err != nil {
		return fmt.Errorf("player_id: %w", err)
	}
	checkin, err := canonicalUUID(c.CheckinID)
	if err != nil {
		return fmt.Errorf("checkin_id: %w", err)
	}
	c.PlayerID, c.CheckinID = player, checkin
	return nil
}

var errNotUUID = errors.New("must be a UUID")

// canonicalUUID accepts the 36-character form only. uuid.Parse also takes
// braces, a urn: prefix and no dashes; Schmetterpause sends none of them, and
// accepting them would only make two spellings of one id.
func canonicalUUID(s string) (string, error) {
	if len(s) != 36 {
		return "", errNotUUID
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return "", errNotUUID
	}
	return id.String(), nil
}

// ClaimByPhone claims a half for the player of a phone check-in, against
// table. Everything after the door is the fob's claim: the same halves,
// timeout, replace and move rules, and the second half starts the match.
//
// A claim with a check-in id seen before claims nothing and gets the first
// answer again, with Repeated set, waiting for it while it is under way.
func (d *Desk) ClaimByPhone(ctx context.Context, c PhoneClaim, table Table) Answer {
	attrs := []any{"via", "phone", "half", c.Half}

	d.mu.Lock()
	d.expire(d.now())

	if prev := d.phone[c.CheckinID]; prev != nil {
		halves := d.halves()
		d.mu.Unlock()
		select {
		case <-prev.done:
		case <-ctx.Done():
			return Answer{Outcome: Unavailable, Half: c.Half, Halves: halves,
				Err: fmt.Errorf("waiting for the first attempt: %w", ctx.Err())}
		}
		a := prev.answer
		a.Repeated = true
		d.log.Info("check-in repeated", append(attrs, "outcome", a.Outcome)...)
		return a
	}

	att := &attempt{done: make(chan struct{})}
	d.rememberPhone(c.CheckinID, att)

	if table.Occupied() {
		a := d.finish(att, attrs, Answer{Outcome: Refused, Half: c.Half, Err: errRunning})
		d.mu.Unlock()
		return a
	}
	d.mu.Unlock()

	// The name is a network call, so it is fetched without the lock, and not
	// cancelled with the request: a retry waits for this attempt's answer,
	// so the attempt has to reach one.
	name := d.nameOf(context.WithoutCancel(ctx), c.PlayerID)

	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	d.expire(now)
	return d.settle(att, attrs, c.Half,
		Claim{PlayerID: c.PlayerID, CheckinID: c.CheckinID, Name: name, At: now}, table)
}

// rememberPhone records the attempt for a check-in id and forgets the oldest
// once there are too many. Must be called with the lock held.
func (d *Desk) rememberPhone(id string, att *attempt) {
	if len(d.phoneOrder) >= maxPhoneClaims {
		delete(d.phone, d.phoneOrder[0])
		d.phoneOrder = d.phoneOrder[1:]
	}
	d.phone[id] = att
	d.phoneOrder = append(d.phoneOrder, id)
}
