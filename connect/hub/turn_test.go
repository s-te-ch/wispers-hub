// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

package hubsrv

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"connect/client/proto/hubpb"
	"connect/proto/bepb"

	"connect/shared/turncreds"
)

// fakeBE implements the two BackendClient methods GetStunTurnConfig needs;
// everything else panics via the embedded nil interface.
type fakeBE struct {
	bepb.BackendClient
	policy    *bepb.GetTurnPolicyResponse
	policyErr error
}

func (f *fakeBE) AuthenticateNode(ctx context.Context, req *bepb.AuthenticateNodeRequest,
	opts ...grpc.CallOption) (*bepb.AuthenticateNodeResponse, error) {
	return &bepb.AuthenticateNodeResponse{}, nil
}

func (f *fakeBE) GetTurnPolicy(ctx context.Context, req *bepb.GetTurnPolicyRequest,
	opts ...grpc.CallOption) (*bepb.GetTurnPolicyResponse, error) {
	return f.policy, f.policyErr
}

func authedCtx() context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-connectivity-group-id", "11111111-2222-3333-4444-555555555555",
		"x-node-number", "1",
		"x-auth-token", "token",
	))
}

var secret = []byte("hub-test-secret")

func turnServer(be bepb.BackendClient) *HubServer {
	return NewHubServer(be, "stun.test:3478", "relay.test:3478", secret, 0)
}

func TestGetStunTurnConfigMintsForEntitledPlan(t *testing.T) {
	s := turnServer(&fakeBE{policy: &bepb.GetTurnPolicyResponse{
		OrganisationId: "0d2384a1-aaaa-bbbb-cccc-ddddeeeeffff",
		FloorBps:       10_000_000,
		CeilBps:        50_000_000,
		Enabled:        true,
	}})
	cfg, err := s.GetStunTurnConfig(authedCtx(), &hubpb.StunTurnConfigRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TurnServer != "relay.test:3478" || cfg.StunServer != "stun.test:3478" {
		t.Fatalf("servers: %+v", cfg)
	}
	cred, err := turncreds.Verify(secret, cfg.TurnUsername, cfg.TurnPassword)
	if err != nil {
		t.Fatalf("minted credential does not verify: %v", err)
	}
	if cred.Subject != "org-0d2384a1-aaaa-bbbb-cccc-ddddeeeeffff" ||
		cred.FloorBps != 10_000_000 || cred.CeilBps != 50_000_000 {
		t.Fatalf("cred = %+v", cred)
	}
	if cfg.ExpiresAtMillis != cred.Expiry.UnixMilli() {
		t.Error("ExpiresAtMillis does not match credential expiry")
	}
	ttl := time.Until(cred.Expiry)
	if ttl < 9*time.Minute || ttl > 11*time.Minute {
		t.Errorf("TTL %v, want ~10m", ttl)
	}
}

// The standalone version: enabled with no rates mints a credential without
// rate segments (subject only), which coturn accepts and our relay treats
// as unlimited.
func TestGetStunTurnConfigMintsUnlimitedWithoutRates(t *testing.T) {
	s := turnServer(&fakeBE{policy: &bepb.GetTurnPolicyResponse{
		OrganisationId: "standalone-instance", Enabled: true,
	}})
	cfg, err := s.GetStunTurnConfig(authedCtx(), &hubpb.StunTurnConfigRequest{})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := turncreds.Verify(secret, cfg.TurnUsername, cfg.TurnPassword)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Subject != "org-standalone-instance" || cred.FloorBps != 0 || cred.CeilBps != 0 {
		t.Fatalf("cred = %+v", cred)
	}
}

func TestGetStunTurnConfigStunOnlyWithoutEntitlement(t *testing.T) {
	s := turnServer(&fakeBE{policy: &bepb.GetTurnPolicyResponse{
		OrganisationId: "x", FloorBps: 0, CeilBps: 0,
	}})
	cfg, err := s.GetStunTurnConfig(authedCtx(), &hubpb.StunTurnConfigRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TurnServer != "" || cfg.TurnUsername != "" || cfg.StunServer == "" {
		t.Fatalf("want STUN-only, got %+v", cfg)
	}
}

// A be failure propagates: hiding it behind a STUN-only fallback would
// mask backend problems.
func TestGetStunTurnConfigFailsOnBackendError(t *testing.T) {
	s := turnServer(&fakeBE{policyErr: errors.New("be down")})
	if _, err := s.GetStunTurnConfig(authedCtx(), &hubpb.StunTurnConfigRequest{}); err == nil {
		t.Fatal("backend error was swallowed")
	}
}

// No -turn-server configured: STUN-only without even consulting be.
func TestGetStunTurnConfigTurnDisabled(t *testing.T) {
	s := NewHubServer(&fakeBE{policyErr: errors.New("must not be called")}, "stun.test:3478", "", nil, 0)
	cfg, err := s.GetStunTurnConfig(authedCtx(), &hubpb.StunTurnConfigRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TurnServer != "" || cfg.TurnUsername != "" {
		t.Fatalf("want no TURN, got %+v", cfg)
	}
}
