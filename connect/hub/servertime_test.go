// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

package servertime

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

// TestStampServerTime runs a real unary RPC through the interceptor: the
// header is set after the handler returns, so only an end-to-end call shows
// whether grpc-go still delivers it.
func TestStampServerTime(t *testing.T) {
	const delay = 20 * time.Millisecond
	slow := grpc.ChainUnaryInterceptor(func(
		ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
	) (any, error) {
		time.Sleep(delay)
		return handler(ctx, req)
	})
	// Interceptor under test outermost (as in production), so the slow
	// inner interceptor stands in for the rest of the server-side chain.
	opts := append(Interceptors(), slow)
	srv := grpc.NewServer(opts...)
	healthpb.RegisterHealthServer(srv, health.NewServer())
	lis := bufconn.Listen(1 << 16)
	go srv.Serve(lis)
	defer srv.Stop()

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var header, trailer metadata.MD
	_, err = healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{},
		grpc.Header(&header), grpc.Trailer(&trailer))
	if err != nil {
		t.Fatal(err)
	}
	vals := header.Get(Key)
	if len(vals) != 1 {
		t.Fatalf("header %s = %v (trailer %v), want exactly one value", Key, vals, trailer.Get(Key))
	}
	us, err := strconv.ParseInt(vals[0], 10, 64)
	if err != nil {
		t.Fatalf("header %s = %q, want integer microseconds: %v", Key, vals[0], err)
	}
	if got := time.Duration(us) * time.Microsecond; got < delay || got > delay+time.Second {
		t.Errorf("server time = %v, want >= %v (handler sleep)", got, delay)
	}
}
