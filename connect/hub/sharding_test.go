// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

package sharding

import (
	"testing"
	"time"
)

// The fixture table from exp/sharding — the hub rule must mirror the
// frontend matcher exactly, else-cases included.
func TestGetShard(t *testing.T) {
	for _, tc := range []struct {
		cgID string
		want int
	}{
		{"0aaaaaaa-1111-4111-8111-222222222222", 1},
		{"7aaaaaaa-1111-4111-8111-222222222222", 1},
		{"9aaaaaaa-1111-4111-8111-222222222222", 1},
		{"caaaaaaa-1111-4111-8111-222222222222", 1},
		{"daaaaaaa-1111-4111-8111-222222222222", 2},
		{"eaaaaaaa-1111-4111-8111-222222222222", 2},
		{"faaaaaaa-1111-4111-8111-222222222222", 2},
		{"Baaaaaaa-1111-4111-8111-222222222222", 2}, // uppercase → else
		{"not-a-uuid", 2},
		{"", 2},
	} {
		if got := GetShard(tc.cgID); got != tc.want {
			t.Errorf("GetShard(%q) = %d, want %d", tc.cgID, got, tc.want)
		}
	}
}

func TestShedFires(t *testing.T) {
	restore := shrinkTTL(t)
	defer restore()

	closed := make(chan struct{})
	stop, shed := ShedAfterTTL("daaaaaaa-1111-4111-8111-222222222222", 1, func() { close(closed) })
	defer stop()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("closeConn not called")
	}
	if !shed.Load() {
		t.Error("shed not set")
	}
}

func TestStopDisarms(t *testing.T) {
	oldMin, oldMax := TTLMin, TTLMax
	TTLMin, TTLMax = 40*time.Millisecond, 41*time.Millisecond
	defer func() { TTLMin, TTLMax = oldMin, oldMax }()

	stop, shed := ShedAfterTTL("daaaaaaa-1111-4111-8111-222222222222", 1, func() {
		t.Error("closeConn called after stop")
	})
	stop()
	time.Sleep(80 * time.Millisecond)
	if shed.Load() {
		t.Error("shed set after stop")
	}
}

func shrinkTTL(t *testing.T) (restore func()) {
	t.Helper()
	oldMin, oldMax := TTLMin, TTLMax
	TTLMin, TTLMax = 5*time.Millisecond, 6*time.Millisecond
	return func() { TTLMin, TTLMax = oldMin, oldMax }
}
