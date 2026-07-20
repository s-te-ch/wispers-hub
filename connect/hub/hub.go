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

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
)

var (
	grpcPort = flag.Int("grpc-port", 50051, "The gRPC port")
	restPort = flag.Int("rest-port", 2357, "The REST port (only used in standalone mode)")
	beAddr   = flag.String("be", "be:50051", "Backend gRPC address, used in managed mode")
	dbPath   = flag.String("db", "", "Override for the state DB path, used in standalone mode")
	stunAddr = flag.String("stun-server", "", "STUN server host:port. Required in standalone mode.")
)

// The hosted STUN server, used only as the managed-mode default.
// Standalone operators must configure their own.
const managedDefaultStunServer = "stun.wispers.dev:3478"

func main() {
	runMode := Standalone
	flag.Var(&runMode, "mode", "Run mode (standalone|managed)")
	flag.Parse()
	var err error
	if runMode == Managed {
		err = runManaged()
	} else {
		err = runStandalone()
	}
	if err != nil {
		log.Fatal(err)
	}
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
	return serveHub(hubsrv.NewHubServer(bepb.NewBackendClient(beConn), stunServer))
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
	hubServer := hubsrv.NewHubServer(beClient, *stunAddr)

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
	grpcServer := grpc.NewServer(hubsrv.VersionInterceptors()...)
	hubpb.RegisterHubServer(grpcServer, hubServer)
	const legacyName = "connect.hub.Hub"
	if legacy := hubpb.Hub_ServiceDesc; legacy.ServiceName != legacyName {
		legacy.ServiceName = legacyName
		grpcServer.RegisterService(&legacy, hubServer)
	}
	reflection.Register(grpcServer)
	return grpcServer
}
