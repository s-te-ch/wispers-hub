// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

// The frontends shard hub traffic by the first hex digit of the connectivity
// group ID. This package is the hub-side mirror of that rule. It MUST stay in
// lockstep with the matcher in the frontend Caddyfiles.
//
// The hub needs to know about the sharding function because it distinguishes
// between connections that belong to its assigned shard and connections that
// belong to another shard but have failed over. These failed over guest
// connections must be closed after a while to cause them to try failing back.
package sharding

import (
	"log"
	"math/rand/v2"
	"sync/atomic"
	"time"
)

// GetShard returns the shard a connectivity group id belongs to,
// currently 1 for first hex digit [0-9a-c] (~80%), 2 for everything else.
func GetShard(cgID string) int {
	if cgID == "" {
		return 2
	}
	c := cgID[0]
	if ('0' <= c && c <= '9') || ('a' <= c && c <= 'c') {
		return 1
	}
	return 2
}

// ShedAfterTTL schedules the connection to be shed after a random TTL. This
// causes a non-owned serving stream to reconnect, failing back to the original
// hub if it's serving.
//
// The returned stop func disarms the shedding, shed reports whether the TTL
// fired.
func ShedAfterTTL(cgID string, nodeNum int32, closeConn func()) (stop func(), shed *atomic.Bool) {
	fired := new(atomic.Bool)
	delay := ttl()
	timer := time.AfterFunc(delay, func() {
		fired.Store(true)
		log.Printf("Shard TTL: closing stream of node %d in non-owned group %s after %v",
			nodeNum, cgID, delay.Round(time.Second))
		closeConn()
	})
	return func() { timer.Stop() }, fired
}

// Parameters for ttl(), exposed as vars for testing.
var (
	TTLMin = 2 * time.Minute
	TTLMax = 5 * time.Minute
)

// ttl returns the time to live for connections that have failed over from
// another hub. The TTL is randomised in order to prevent a thundering herd when
// the original hub comes back up and its connections fail back.
func ttl() time.Duration {
	return TTLMin + rand.N(TTLMax-TTLMin)
}
