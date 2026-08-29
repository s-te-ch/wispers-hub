// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

// End-to-end shard-TTL behavior through StartServing: the shed surfaces
// as the resharding status, owned streams survive. The rule and timer
// mechanics themselves are covered in the sharding package's tests.
package hubsrv

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"connect/client/proto/hubpb"
	"connect/hub/sharding"
	"connect/proto/bepb"
)

func TestStartServingShedsNonOwnedStream(t *testing.T) {
	restore := shrinkShardTTL(t)
	defer restore()

	// daaa… is in shard 2; this hub is assigned shard 1.
	err := runServing(t, 1, "daaaaaaa-1111-4111-8111-222222222222", "0.14.0", nil)
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "resharding") {
		t.Fatalf("want Unavailable resharding error, got %v", err)
	}
}

func TestStartServingKeepsOwnedStream(t *testing.T) {
	restore := shrinkShardTTL(t)
	defer restore()

	// 1aaa… is in shard 1 == this hub's: the stream must outlive
	// many TTLs and end only on client disconnect, without error.
	clientGone := make(chan struct{})
	go func() {
		time.Sleep(80 * time.Millisecond)
		close(clientGone)
	}()
	if err := runServing(t, 1, "1aaaaaaa-1111-4111-8111-222222222222", "0.14.0", clientGone); err != nil {
		t.Fatalf("owned stream ended with error: %v", err)
	}
}

func TestStartServingSparesPreCheckInClient(t *testing.T) {
	restore := shrinkShardTTL(t)
	defer restore()

	// Non-owned group, but the client predates CheckIn (no version
	// header): shedding it would create a zombie, so it must be
	// spared and end only on client disconnect.
	clientGone := make(chan struct{})
	go func() {
		time.Sleep(80 * time.Millisecond)
		close(clientGone)
	}()
	if err := runServing(t, 1, "daaaaaaa-1111-4111-8111-222222222222", "", clientGone); err != nil {
		t.Fatalf("pre-CheckIn stream ended with error: %v", err)
	}
}

func TestStartServingKeepsStreamWithoutAssignedShard(t *testing.T) {
	restore := shrinkShardTTL(t)
	defer restore()

	clientGone := make(chan struct{})
	go func() {
		time.Sleep(80 * time.Millisecond)
		close(clientGone)
	}()
	if err := runServing(t, 0, "daaaaaaa-1111-4111-8111-222222222222", "0.14.0", clientGone); err != nil {
		t.Fatalf("shardless stream ended with error: %v", err)
	}
}

func shrinkShardTTL(t *testing.T) (restore func()) {
	t.Helper()
	oldMin, oldMax := sharding.TTLMin, sharding.TTLMax
	sharding.TTLMin, sharding.TTLMax = 5*time.Millisecond, 6*time.Millisecond
	return func() { sharding.TTLMin, sharding.TTLMax = oldMin, oldMax }
}

// runServing drives StartServing on a hub with the given assigned
// shard and a fake client stream, returning what StartServing returns.
// If clientGone is non-nil, the fake client hangs up when it closes;
// otherwise the stream only ends server-side. A watchdog fails the
// test if StartServing never returns.
func runServing(t *testing.T, assignedShard int, cgID string, clientVersion string, clientGone chan struct{}) error {
	t.Helper()
	s := NewHubServer(&shardFakeBE{}, "stun.test:3478", "", nil, assignedShard)
	md := metadata.Pairs(
		"x-connectivity-group-id", cgID,
		"x-node-number", "1",
		"x-auth-token", "token",
	)
	if clientVersion != "" {
		md.Set("wispers-client-version", clientVersion)
	}
	ctx, cancel := context.WithCancel(metadata.NewIncomingContext(context.Background(), md))
	defer cancel()
	if clientGone != nil {
		go func() {
			<-clientGone
			cancel()
		}()
	}

	done := make(chan error, 1)
	go func() {
		done <- s.StartServing(&fakeServingStream{ctx: ctx})
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("StartServing did not return")
		return nil
	}
}

// shardFakeBE accepts any node; everything StartServing touches is
// stubbed, the rest panics via the embedded nil interface.
type shardFakeBE struct {
	bepb.BackendClient
}

func (*shardFakeBE) AuthenticateNode(ctx context.Context, req *bepb.AuthenticateNodeRequest,
	opts ...grpc.CallOption) (*bepb.AuthenticateNodeResponse, error) {
	return &bepb.AuthenticateNodeResponse{}, nil
}

func (*shardFakeBE) UpdateNodeLastSeen(ctx context.Context, req *bepb.UpdateNodeLastSeenRequest,
	opts ...grpc.CallOption) (*bepb.UpdateNodeLastSeenResponse, error) {
	return &bepb.UpdateNodeLastSeenResponse{}, nil
}

func (*shardFakeBE) LogActivity(ctx context.Context, req *bepb.LogActivityRequest,
	opts ...grpc.CallOption) (*bepb.LogActivityResponse, error) {
	return &bepb.LogActivityResponse{}, nil
}

// fakeServingStream is the minimal Hub_StartServingServer: Send
// succeeds, Recv blocks until the stream context ends.
type fakeServingStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeServingStream) Context() context.Context { return f.ctx }

func (f *fakeServingStream) Send(*hubpb.ServingRequest) error { return nil }

func (f *fakeServingStream) Recv() (*hubpb.ServingResponse, error) {
	<-f.ctx.Done()
	return nil, f.ctx.Err()
}
