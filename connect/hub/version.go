// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

package hubsrv

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Version is the hub's release version, reported to clients in the
// wispers-hub-version response header. The copybara export enforces
// that this matches the version in MODULE.bazel.
const Version = "0.4.0"

// MinClientVersion is this hub's compatibility floor for client versions.
const MinClientVersion = "0.0.0"

// versionRejectionPrefix starts every floor-rejection message. The client
// library matches on it to classify the error; keep in sync with hub.rs.
const versionRejectionPrefix = "incompatible client version"

const (
	clientVersionKey = "wispers-client-version"
	hubVersionKey    = "wispers-hub-version"
)

// VersionInterceptors returns server options that advertise the hub's
// version on every response and enforce MinClientVersion, so that skewed
// deployments fail with an explicit version message instead of an obscure
// error.
func VersionInterceptors() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(func(
			ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
		) (any, error) {
			grpc.SetHeader(ctx, metadata.Pairs(hubVersionKey, Version))
			if err := checkClientVersion(ctx); err != nil {
				return nil, err
			}
			return handler(ctx, req)
		}),
		grpc.ChainStreamInterceptor(func(
			srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler,
		) error {
			ss.SetHeader(metadata.Pairs(hubVersionKey, Version))
			if err := checkClientVersion(ss.Context()); err != nil {
				return err
			}
			return handler(srv, ss)
		}),
	}
}

// checkClientVersion rejects calls from clients below MinClientVersion.
func checkClientVersion(ctx context.Context) error {
	reported, err := extractClientVersion(ctx)
	if err != nil {
		return err
	}
	floor := minClientParsed
	if versionLess(reported, floor) {
		return status.Errorf(codes.FailedPrecondition,
			"%s: this hub (v%s) requires wispers-client v%s or newer, got v%s; please upgrade the client",
			versionRejectionPrefix, Version, formatVersion(floor), formatVersion(reported))
	}
	return nil
}

func extractClientVersion(ctx context.Context) ([3]int, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get(clientVersionKey)
	if len(vals) == 0 {
		// No version header, this must be an old client. Return v0.0.0.
		return [3]int{0, 0, 0}, nil
	}
	v, err := parseVersion(vals[0])
	if err != nil {
		return v, status.Errorf(codes.InvalidArgument,
			"malformed %s %q: %v", clientVersionKey, vals[0], err)
	}
	return v, nil
}

var minClientParsed = mustParseVersion(MinClientVersion)

func formatVersion(v [3]int) string {
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

// versionLess reports whether version a precedes version b.
func versionLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// parseVersion parses "major.minor.patch". A pre-release suffix after '-' is
// ignored: for floor purposes, 0.12.0-rc1 counts as 0.12.0.
func parseVersion(s string) ([3]int, error) {
	base, _, _ := strings.Cut(s, "-")
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return [3]int{}, fmt.Errorf("want major.minor.patch")
	}
	var v [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, fmt.Errorf("bad component %q", p)
		}
		v[i] = n
	}
	return v, nil
}

func mustParseVersion(s string) [3]int {
	v, err := parseVersion(s)
	if err != nil {
		panic(fmt.Sprintf("bad version constant %q: %v", s, err))
	}
	return v
}
