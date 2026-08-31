// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

// Package servertime stamps every unary response with the hub's server-side
// processing time, so clients can subtract it from an observed request
// duration and get a clean network round-trip estimate out of whichever RPC
// they happened to send.
package servertime

import (
	"context"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// Key is the metadata key. The value is processing time in microseconds.
const Key = "wispers-server-time-us"

// Interceptors returns server options, to be composed with others when building
// the gRPC server.
func Interceptors() []grpc.ServerOption {
	return []grpc.ServerOption{grpc.ChainUnaryInterceptor(measureAndStamp)}
}

// measureAndStamp measures the latency of everything inward of this interceptor
// and stamps the result as a response header.
//
// For unary RPCs grpc-go flushes headers only
// when the response message or status goes out, which happens after this
// returns, so setting the header post-handler still lands. Should a handler
// ever flush headers itself, fall back to the trailer, which tonic merges
// into the unary response metadata like a header.
func measureAndStamp(
	ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
) (any, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	md := metadata.Pairs(Key, strconv.FormatInt(time.Since(start).Microseconds(), 10))
	if grpc.SetHeader(ctx, md) != nil {
		// An inner handler already flushed headers. Fall back to the trailer,
		// which client-side's tonic merges into the response metadata.
		grpc.SetTrailer(ctx, md)
	}
	return resp, err
}
