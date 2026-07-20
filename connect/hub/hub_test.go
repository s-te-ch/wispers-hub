// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"maps"
	"net"
	"slices"
	"testing"

	"connect/client/proto/hubpb"
	"connect/hub/hubsrv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// Nodes running pre-rename clients call the hub as connect.hub.Hub. The hub
// must answer to that name until 1.0, whichever client proto version it was
// built against (post-rename via the alias, pre-rename natively).
func TestHubServesLegacyServiceName(t *testing.T) {
	srv := newHubGRPCServer(hubsrv.NewHubServer(nil, "stun.example.invalid:3478"))
	info := srv.GetServiceInfo()
	if _, ok := info["connect.hub.Hub"]; !ok {
		t.Errorf("hub gRPC server does not serve connect.hub.Hub; services: %v",
			slices.Collect(maps.Keys(info)))
	}
}

// The client version test is currently not armed, because clients that report
// their version are not yet widely deployed. For now, test what's testable
// (invalid version strings etc.)
func TestHubVersionGate(t *testing.T) {
	srv := newHubGRPCServer(hubsrv.NewHubServer(nil, "stun.example.invalid:3478"))
	lis := bufconn.Listen(1 << 20)
	go srv.Serve(lis)
	defer srv.Stop()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()
	client := hubpb.NewHubClient(conn)

	call := func(clientVersion string) (metadata.MD, *status.Status) {
		ctx := context.Background()
		if clientVersion != "" {
			ctx = metadata.AppendToOutgoingContext(ctx,
				"wispers-client-version", clientVersion)
		}
		var header metadata.MD
		_, err := client.ListNodes(ctx, &hubpb.ListNodesRequest{}, grpc.Header(&header))
		return header, status.Convert(err)
	}

	// Malformed version: an explicit complaint, not a guessing game.
	if _, st := call("yolo"); st.Code() != codes.InvalidArgument {
		t.Errorf("malformed version: got %v %q, want InvalidArgument", st.Code(), st.Message())
	}

	// A versioned client: past the gate (fails authentication instead), and
	// the response carries the hub's version.
	header, st := call("0.12.0")
	if st.Code() != codes.Unauthenticated {
		t.Errorf("versioned client: got %v %q, want Unauthenticated (past the version gate)",
			st.Code(), st.Message())
	}
	if got := header.Get("wispers-hub-version"); len(got) != 1 || got[0] != hubsrv.Version {
		t.Errorf("wispers-hub-version header: got %v, want [%s]", got, hubsrv.Version)
	}

	// No version header, i.e. a client from before version signaling: counts
	// as v0.0.0, which the disarmed floor admits.
	if _, st := call(""); st.Code() != codes.Unauthenticated {
		t.Errorf("pre-signaling client: got %v %q, want Unauthenticated (past the version gate)",
			st.Code(), st.Message())
	}
}
