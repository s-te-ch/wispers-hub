// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

package hubsrv

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRecordUnaryMetrics(t *testing.T) {
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Svc/Unary"}
	handler := func(ctx context.Context, req any) (any, error) {
		return "ok", status.Error(codes.FailedPrecondition, "nope")
	}
	if _, err := recordUnaryMetrics(context.Background(), nil, info, handler); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("interceptor altered the error: %v", err)
	}

	if got := testutil.ToFloat64(grpcStarted.WithLabelValues("unary", "test.Svc", "Unary")); got != 1 {
		t.Errorf("started = %v, want 1", got)
	}
	if got := testutil.ToFloat64(grpcHandled.WithLabelValues("unary", "test.Svc", "Unary", "FailedPrecondition")); got != 1 {
		t.Errorf("handled{FailedPrecondition} = %v, want 1", got)
	}
}

func TestRecordStreamMetrics(t *testing.T) {
	info := &grpc.StreamServerInfo{
		FullMethod:     "/test.Svc/Watch",
		IsClientStream: true,
		IsServerStream: true,
	}
	handler := func(srv any, ss grpc.ServerStream) error {
		var msg string
		if err := ss.RecvMsg(&msg); err != nil {
			return err
		}
		return ss.SendMsg("tick")
	}
	if err := recordStreamMetrics(nil, fakeStream{}, info, handler); err != nil {
		t.Fatalf("recordStreamMetrics: %v", err)
	}

	for name, vec := range map[string]float64{
		"started":  testutil.ToFloat64(grpcStarted.WithLabelValues("bidi_stream", "test.Svc", "Watch")),
		"handled":  testutil.ToFloat64(grpcHandled.WithLabelValues("bidi_stream", "test.Svc", "Watch", "OK")),
		"received": testutil.ToFloat64(grpcMsgReceived.WithLabelValues("bidi_stream", "test.Svc", "Watch")),
		"sent":     testutil.ToFloat64(grpcMsgSent.WithLabelValues("bidi_stream", "test.Svc", "Watch")),
	} {
		if vec != 1 {
			t.Errorf("%s = %v, want 1", name, vec)
		}
	}
}

func TestSplitMethodName(t *testing.T) {
	for _, tc := range []struct {
		full, service, method string
	}{
		{"/wispers.connect.hub.Hub/StartServing", "wispers.connect.hub.Hub", "StartServing"},
		{"garbage", "unknown", "garbage"},
	} {
		service, method := splitMethodName(tc.full)
		if service != tc.service || method != tc.method {
			t.Errorf("splitMethodName(%q) = %q, %q; want %q, %q",
				tc.full, service, method, tc.service, tc.method)
		}
	}
}

// fakeStream is the minimal ServerStream for driving the interceptor.
type fakeStream struct {
	grpc.ServerStream
}

func (fakeStream) Context() context.Context { return context.Background() }
func (fakeStream) RecvMsg(m any) error      { return nil }
func (fakeStream) SendMsg(m any) error      { return nil }
