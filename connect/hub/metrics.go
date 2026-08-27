// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

package hubsrv

import (
	"context"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// Metric names and labels follow the grpc-ecosystem convention.
// Open streams per method = started - sum over codes of handled.
var (
	grpcLabels  = []string{"grpc_type", "grpc_service", "grpc_method"}
	grpcStarted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "grpc_server_started_total",
		Help: "RPCs started on the server.",
	}, grpcLabels)
	grpcHandled = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "grpc_server_handled_total",
		Help: "RPCs completed on the server, by status code.",
	}, append(grpcLabels, "grpc_code"))
	grpcMsgReceived = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "grpc_server_msg_received_total",
		Help: "Stream messages received by the server.",
	}, grpcLabels)
	grpcMsgSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "grpc_server_msg_sent_total",
		Help: "Stream messages sent by the server.",
	}, grpcLabels)
	grpcHandling = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "grpc_server_handling_seconds",
		Help: "RPC duration; for streams, the whole stream lifetime.",
		// Serving streams live for hours, so the buckets extend far
		// beyond the default 10s ceiling.
		Buckets: []float64{.001, .01, .1, 1, 10, 60, 600, 3600, 21600, 86400},
	}, grpcLabels)
)

// MetricsInterceptors returns server options that record the
// grpc_server_* metrics for every RPC. Order them _before_ other
// interceptors so rejected calls get counted too.
func MetricsInterceptors() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(recordUnaryMetrics),
		grpc.ChainStreamInterceptor(recordStreamMetrics),
	}
}

func recordUnaryMetrics(
	ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
) (any, error) {
	service, method := splitMethodName(info.FullMethod)
	grpcStarted.WithLabelValues("unary", service, method).Inc()
	start := time.Now()
	resp, err := handler(ctx, req)
	grpcHandling.WithLabelValues("unary", service, method).Observe(time.Since(start).Seconds())
	grpcHandled.WithLabelValues("unary", service, method, status.Code(err).String()).Inc()
	return resp, err
}

func recordStreamMetrics(
	srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler,
) error {
	streamType := "bidi_stream"
	switch {
	case info.IsClientStream && !info.IsServerStream:
		streamType = "client_stream"
	case !info.IsClientStream && info.IsServerStream:
		streamType = "server_stream"
	}
	service, method := splitMethodName(info.FullMethod)
	grpcStarted.WithLabelValues(streamType, service, method).Inc()
	start := time.Now()
	err := handler(srv, countingStream{
		ServerStream: ss,
		received:     grpcMsgReceived.WithLabelValues(streamType, service, method),
		sent:         grpcMsgSent.WithLabelValues(streamType, service, method),
	})
	grpcHandling.WithLabelValues(streamType, service, method).Observe(time.Since(start).Seconds())
	grpcHandled.WithLabelValues(streamType, service, method, status.Code(err).String()).Inc()
	return err
}

// splitMethodName splits "/pkg.Service/Method" into service and method.
func splitMethodName(fullMethod string) (service, method string) {
	full := strings.TrimPrefix(fullMethod, "/")
	if service, method, ok := strings.Cut(full, "/"); ok {
		return service, method
	}
	return "unknown", full
}

// countingStream counts the messages crossing a server stream.
type countingStream struct {
	grpc.ServerStream
	received, sent prometheus.Counter
}

func (s countingStream) RecvMsg(m any) error {
	err := s.ServerStream.RecvMsg(m)
	if err == nil {
		s.received.Inc()
	}
	return err
}

func (s countingStream) SendMsg(m any) error {
	err := s.ServerStream.SendMsg(m)
	if err == nil {
		s.sent.Inc()
	}
	return err
}
