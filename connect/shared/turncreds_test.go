// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

package turncreds

import (
	"strings"
	"testing"
	"time"
)

var secret = []byte("test-secret")

func TestRoundtripManaged(t *testing.T) {
	expiry := time.Unix(1770000000, 0)
	user, pass := Mint(secret, "org-42", 10_000_000, 50_000_000, expiry)
	if user != "1770000000:org-42:10000000:50000000" {
		t.Fatalf("username = %q", user)
	}
	cred, err := Verify(secret, user, pass)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Subject != "org-42" || cred.FloorBps != 10_000_000 || cred.CeilBps != 50_000_000 || !cred.Expiry.Equal(expiry) {
		t.Fatalf("cred = %+v", cred)
	}
}

func TestRoundtripStandalone(t *testing.T) {
	user, pass := Mint(secret, "hub", 0, 0, time.Unix(1770000000, 0))
	if user != "1770000000:hub" {
		t.Fatalf("username = %q", user)
	}
	cred, err := Verify(secret, user, pass)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Subject != "hub" || cred.FloorBps != 0 || cred.CeilBps != 0 {
		t.Fatalf("cred = %+v", cred)
	}
}

func TestTamperedUsernameRejected(t *testing.T) {
	user, pass := Mint(secret, "org-42", 10, 50, time.Unix(1770000000, 0))
	// Upgrade own ceiling: signature must not verify.
	tampered := strings.Replace(user, ":50", ":500", 1)
	if _, err := Verify(secret, tampered, pass); err == nil {
		t.Fatal("tampered username verified")
	}
	if _, err := Verify([]byte("other"), user, pass); err == nil {
		t.Fatal("wrong secret verified")
	}
}

func TestExpiredIsParseableButExpired(t *testing.T) {
	// Expired credentials must still verify/parse — expiry enforcement is
	// per-method (Allocate only), decided by the caller.
	expiry := time.Unix(1000, 0)
	user, pass := Mint(secret, "org-42", 10, 50, expiry)
	cred, err := Verify(secret, user, pass)
	if err != nil {
		t.Fatal(err)
	}
	if !cred.Expired(time.Unix(2000, 0)) {
		t.Fatal("should be expired")
	}
	if cred.Expired(time.Unix(500, 0)) {
		t.Fatal("should not be expired yet")
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if cred, err := Parse("1:s:10:0"); err != nil || cred.FloorBps != 10 || cred.CeilBps != 0 {
		t.Errorf("floor-only credential should parse (no-ceiling semantics): %+v, %v", cred, err)
	}
	for _, u := range []string{"", "123", "abc:def", "123:", "1:2:3", "1:s:x:9", "1:s:9:x", "1:s:50:10", "1:s:-1:5", "1:s:1:2:3"} {
		if _, err := Parse(u); err == nil {
			t.Errorf("Parse(%q) succeeded", u)
		}
	}
}
