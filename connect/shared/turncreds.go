// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

// TURN credential minting and verification, wire-compatible with coturn's
// use-auth-secret mechanism:
//
//	username = "<expiry-unix-seconds>:<subject>[:<floor-bps>:<ceil-bps>]"
//	password = base64(hmac-sha1(secret, username))
//
// The expiry comes first so coturn's parser finds it. The rest of the
// username is opaque to coturn but carries the bandwidth policy for our own
// relay.
package turncreds

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Credential is the policy carried in a verified TURN username.
type Credential struct {
	Subject  string // e.g. "org-42"; the bandwidth-accounting subject
	FloorBps int64  // guaranteed rate; 0 if the username has no rate segments
	CeilBps  int64  // best-effort ceiling; 0 if the username has no rate segments
	Expiry   time.Time
}

// Expired reports whether the credential's timestamp has passed.
func (c *Credential) Expired(now time.Time) bool {
	return now.After(c.Expiry)
}

// Mint creates a credential pair. floorBps/ceilBps of 0/0 omits the rate
// segments, giving unlimited bandwidth (except for the network limits).
// The subject must not contain ':'.
func Mint(secret []byte, subject string, floorBps, ceilBps int64, expiry time.Time) (username, password string) {
	if subject == "" || strings.Contains(subject, ":") {
		panic(fmt.Sprintf("turncreds: invalid subject %q", subject))
	}
	if floorBps > 0 || ceilBps > 0 {
		username = fmt.Sprintf("%d:%s:%d:%d", expiry.Unix(), subject, floorBps, ceilBps)
	} else {
		username = fmt.Sprintf("%d:%s", expiry.Unix(), subject)
	}
	return username, Sign(secret, username)
}

// Verify checks the password against the username and parses the policy.
// It does NOT check expiry (see Credential.Expired).
func Verify(secret []byte, username, password string) (*Credential, error) {
	want := Sign(secret, username)
	if subtle.ConstantTimeCompare([]byte(want), []byte(password)) != 1 {
		return nil, errors.New("turncreds: bad signature")
	}
	return Parse(username)
}

// Parse extracts the policy from a username without verifying it.
func Parse(username string) (*Credential, error) {
	parts := strings.Split(username, ":")
	var cred Credential
	switch len(parts) {
	case 4:
		floor, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("turncreds: bad floor: %w", err)
		}
		ceil, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("turncreds: bad ceil: %w", err)
		}
		// Zero ceiling with a floor means "floor guarantee, no ceiling"
		// (the relay may borrow up to node capacity).
		if floor < 0 || (ceil != 0 && ceil < floor) {
			return nil, fmt.Errorf("turncreds: invalid rates %d/%d", floor, ceil)
		}
		cred.FloorBps, cred.CeilBps = floor, ceil
	case 2:
		// no rate segments
	default:
		return nil, fmt.Errorf("turncreds: %d segments", len(parts))
	}
	ts, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("turncreds: bad expiry: %w", err)
	}
	cred.Expiry = time.Unix(ts, 0)
	cred.Subject = parts[1]
	if cred.Subject == "" {
		return nil, errors.New("turncreds: empty subject")
	}
	return &cred, nil
}

// Sign computes the coturn REST API password for a username.
func Sign(secret []byte, username string) string {
	mac := hmac.New(sha1.New, secret)
	mac.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
