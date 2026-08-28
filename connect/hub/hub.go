// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"connect/client/proto/hubpb"
	"connect/hub/hubsrv"
	"connect/hub/standalone"
	"connect/proto/bepb"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

var (
	grpcPort      = flag.Int("grpc-port", 50051, "The gRPC port")
	assignedShard = flag.Int("assigned-shard", 0, "In manged mode, the shard of the cgID key space this hub serves. 0 for everything, 1 or 2 for specific shards")
	restPort      = flag.Int("rest-port", 2357, "The REST port (only used in standalone mode)")
	metricsPort   = flag.Int("metrics-port", 0, "Prometheus metrics HTTP port; 0 disables")
	beAddr        = flag.String("be", "be:50051", "Backend gRPC address, used in managed mode")
	dbPath        = flag.String("db", "", "Override for the state DB path, used in standalone mode")
	stunAddr      = flag.String("stun-server", "", "STUN server host:port. Required in standalone mode.")
	turnAddr      = flag.String("turn-server", "", "TURN relay host:port; empty disables TURN. Secret via WISPERS_TURN_SECRET.")
)

// The hosted STUN server, used only as the managed-mode default.
// Standalone operators must configure their own.
const managedDefaultStunServer = "stun.wispers.dev:3478"

func main() {
	runMode := Standalone
	flag.Var(&runMode, "mode", "Run mode (standalone|managed)")
	flag.Parse()
	var err error
	if err = startMetricsServer(); err != nil {
		log.Fatalf("metrics server: %v", err)
	}
	if runMode == Managed {
		err = runManaged()
	} else {
		err = runStandalone()
	}
	if err != nil {
		log.Fatal(err)
	}
}

// startMetricsServer serves the Prometheus /metrics page.
func startMetricsServer() error {
	if *metricsPort == 0 {
		return nil
	}
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *metricsPort))
	if err != nil {
		return fmt.Errorf("metrics listener: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	log.Printf("Metrics listening at %v", lis.Addr())
	go func() {
		if err := http.Serve(lis, mux); err != nil {
			log.Printf("metrics server: %v", err)
		}
	}()
	return nil
}

func runManaged() error {
	log.Println("Starting in managed mode")

	// Connect to backend
	log.Printf("Connecting to backend at %s", *beAddr)
	beConn, err := grpc.NewClient(*beAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("grpc.NewClient: %w", err)
	}
	defer beConn.Close()

	stunServer := *stunAddr
	if stunServer == "" {
		stunServer = managedDefaultStunServer
	}
	return serveHub(hubsrv.NewHubServer(bepb.NewBackendClient(beConn), stunServer, *turnAddr, readTurnSecretOrDie(), *assignedShard))
}

func runStandalone() error {
	log.Println("Starting in standalone mode")

	if *stunAddr == "" {
		return fmt.Errorf("Missing --stun-server")
	}

	path, err := resolveDBPath()
	if err != nil {
		return err
	}
	log.Printf("State DB at %s", path)
	st, err := standalone.Open(path)
	if err != nil {
		return err
	}
	defer st.Close()
	if st.BootstrapAPIKey != "" {
		log.Printf("=== Initial API key: %s ===", st.BootstrapAPIKey)
		log.Printf("=== (also written to %s — store it, then delete that file) ===", st.BootstrapAPIKeyFile)
	}

	// Run the sqlite backend on an in-memory gRPC transport, so everything
	// downstream is identical to managed mode.
	beClient, stop, err := standalone.Serve(standalone.NewBackend(st))
	if err != nil {
		return err
	}
	defer stop()
	// Standalone hubs are unsharded: no assigned shard, no shard TTLs.
	hubServer := hubsrv.NewHubServer(beClient, *stunAddr, *turnAddr, readTurnSecretOrDie(), 0)

	// Admin REST API for wcadm and waserver. On the hosted version, this is a
	// separate server. The hub server doubles as the online-status oracle: in
	// standalone mode its connection table is authoritative.
	restSrv := &http.Server{
		Addr:    fmt.Sprintf(":%d", *restPort),
		Handler: standalone.NewRESTHandler(st, hubServer.ConnectedNodes),
	}
	defer restSrv.Close()

	// Run both servers; the first error wins and the deferred cleanups take
	// the other server down.
	errCh := make(chan error, 2)
	go func() {
		log.Printf("REST API listening at %s", restSrv.Addr)
		if err := restSrv.ListenAndServe(); err != http.ErrServerClosed {
			errCh <- fmt.Errorf("REST server: %w", err)
		}
	}()
	go func() {
		errCh <- serveHub(hubServer)
	}()
	return <-errCh
}

// resolveDBPath returns the state DB path. Priorities:
//  1. --db override if given
//  2. $STATE_DIRECTORY/hub.db (set by systemd's StateDirectory=)
//  3. $XDG_STATE_HOME/wispers-connect/hub/hub.db
//  4. ~/.local/state/wispers-connect/hub/hub.db
//
// Creates the directory if needed.
func resolveDBPath() (string, error) {
	if *dbPath != "" {
		return *dbPath, nil
	}
	if stateDir := os.Getenv("STATE_DIRECTORY"); stateDir != "" {
		// systemd passes a colon-separated list when StateDirectory= names
		// several directories; use the first.
		dir, _, _ := strings.Cut(stateDir, ":")
		return filepath.Join(dir, "hub.db"), nil
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" || !filepath.IsAbs(stateHome) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("os.UserHomeDir: %w", err)
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(stateHome, "wispers-connect", "hub")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create state directory: %w", err)
	}
	return filepath.Join(dir, "hub.db"), nil
}

// serveHub runs the node-facing hub gRPC server.
func serveHub(hubServer *hubsrv.HubServer) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *grpcPort))
	if err != nil {
		return fmt.Errorf("net.Listen: %w", err)
	}
	defer lis.Close()
	grpcServer := newHubGRPCServer(hubServer)

	log.Printf("Hub listening at %v", lis.Addr())
	if err := grpcServer.Serve(lis); err != nil {
		return fmt.Errorf("grpcServer.Serve: %w", err)
	}
	return nil
}

// newHubGRPCServer registers the hub service, additionally under its
// pre-rename name connect.hub.Hub: clients released before the
// wispers.connect.hub rename call that name. Keep the alias until 1.0.
// The guard makes this a no-op when built against a pre-rename client proto
// (as the wispers-hub repo does until its client pin is bumped), where the
// primary registration already claims the legacy name.
func newHubGRPCServer(hubServer *hubsrv.HubServer) *grpc.Server {

	opts := append(
		// Metrics first so version-rejected calls are counted too.
		hubsrv.MetricsInterceptors(),
		hubsrv.VersionInterceptors()...,
	)
	grpcServer := grpc.NewServer(opts...)
	hubpb.RegisterHubServer(grpcServer, hubServer)
	// Standard health service, for probes.
	healthpb.RegisterHealthServer(grpcServer, health.NewServer())
	const legacyName = "connect.hub.Hub"
	if legacy := hubpb.Hub_ServiceDesc; legacy.ServiceName != legacyName {
		legacy.ServiceName = legacyName
		grpcServer.RegisterService(&legacy, hubServer)
	}
	reflection.Register(grpcServer)
	return grpcServer
}

// readTurnSecretOrDie returns the TURN shared secret from the environment.
func readTurnSecretOrDie() []byte {
	secret := os.Getenv("WISPERS_TURN_SECRET")
	if *turnAddr != "" && secret == "" {
		log.Fatal("--turn-server requires WISPERS_TURN_SECRET")
	}
	return []byte(secret)
}
