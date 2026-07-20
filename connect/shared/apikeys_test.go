// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

package apikeys

import (
	"bytes"
	"crypto/sha256"
	"slices"
	"strings"
	"testing"
)

func TestGenerateWithReaderDeterministic(t *testing.T) {
	src := make([]byte, idBytesLen+secretBytesLen)
	for i := range src {
		src[i] = byte(i + 1)
	}

	details, err := generateWithReader("dev", bytes.NewReader(src))
	if err != nil {
		t.Fatalf("generateWithReader returned error: %v", err)
	}
	_, idPart, secretPart, err := split(details.Encoded)
	if err != nil {
		t.Fatalf("split failed: %v", err)
	}
	if len(idPart) != idEncodedLen {
		t.Fatalf("unexpected key length: got %d want %d", len(idPart), idEncodedLen)
	}
	if len(secretPart) != secretEncodedLen {
		t.Fatalf("unexpected secret length: got %d want %d", len(secretPart), secretEncodedLen)
	}

	if !strings.HasPrefix(details.Encoded, "wc_dev_") {
		t.Fatalf("encoded key missing prefix: %s", details.Encoded)
	}
	wantID := "00P9MVMcaXWHEX8yi"
	if details.ID != wantID {
		t.Fatalf("unexpected key bytes: got %v want %v", details.ID, wantID)
	}
	wantHash := sha256.Sum256(src[idBytesLen:])
	if details.SecretHash != wantHash {
		t.Fatalf("unexpected secret hash: got %x want %x", details.SecretHash, wantHash)
	}

	parsed, err := Parse(details.Encoded)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if parsed.Env != "dev" {
		t.Fatalf("parsed env mismatch: got %s want dev", parsed.Env)
	}
	if parsed.ID != wantID {
		t.Fatalf("parsed key mismatch: got %v want %v", parsed.ID, wantID)
	}
	if parsed.SecretHash != wantHash {
		t.Fatalf("parsed hash mismatch: got %x want %x", parsed.SecretHash, wantHash)
	}
}

func TestParseRejectsInvalidFormats(t *testing.T) {
	cases := []string{
		"",
		"wcdev_invalid_format",
		"wc__missing_env",
		"wc_env_missingdot",
		"wc_env_.secret",
		"wc_env_key.",
		"wc_env_key.secret$",
		"wc_env_short.secret",
		"wc_env_" + strings.Repeat("a", idEncodedLen+1) + "." + strings.Repeat("b", secretEncodedLen),
	}
	for _, raw := range cases {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("expected error for %q", raw)
		}
	}
}

func TestEncodeBase62Padding(t *testing.T) {
	src := make([]byte, idBytesLen)
	// Force leading zeros and a small number.
	src[len(src)-1] = 1
	got := encodeBase62(src, idEncodedLen)
	if len(got) != idEncodedLen {
		t.Fatalf("encodeBase62 returned %d chars, want %d", len(got), idEncodedLen)
	}
	if got[len(got)-1] != '1' {
		t.Fatalf("encodeBase62 did not encode payload correctly: %s", got)
	}
}

func TestEncodeBase62(t *testing.T) {
	s := encodeBase62([]byte{}, 2)
	if s != "00" {
		t.Errorf("Expected '00', got %v", s)
	}
	s = encodeBase62([]byte{61}, 1)
	if s != "z" {
		t.Errorf("Expected z, got %v", s)
	}
	s = encodeBase62([]byte{61}, 3)
	if s != "00z" {
		t.Errorf("Expected 00z, got %v", s)
	}
	s = encodeBase62([]byte{62}, 2)
	if s != "10" {
		t.Errorf("Expected 10, got %v", s)
	}
	s = encodeBase62([]byte{0xe, 0xc6}, 3)
	if s != "0z0" {
		t.Errorf("Expected 0z0, got %v", s)
	}
}

func TestDecodeBase62(t *testing.T) {
	bs, _ := decodeBase62("z", 2)
	if !slices.Equal(bs, []byte{0, 61}) {
		t.Errorf("Expected {0, 61}, got %v", bs)
	}
	bs, _ = decodeBase62("0z0", 2)
	if !slices.Equal(bs, []byte{0xe, 0xc6}) {
		t.Errorf("Expected {0xe, 0xc6}, got %v", bs)
	}
}
