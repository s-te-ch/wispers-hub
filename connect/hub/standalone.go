// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

// State handling for the hub's standalone mode: a single sqlite file instead
// of the hosted backend, but reachable through equivalent REST and gRPC
// interfaces.
package standalone

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"connect/proto/bepb"
	"connect/shared/apikeys"
	"connect/shared/attestation"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	_ "modernc.org/sqlite"
)

//go:embed standalone_schema.sql
var schema string

// Environment segment of standalone API keys (wc_standalone_<id>.<secret>).
const apiKeyEnv = "standalone"

// Key ID for the attestation key, in JWT headers and the JWKS.
const attestationKid = "standalone-1"

// State is the standalone hub's persistent state.
type State struct {
	DB         *sql.DB
	InstanceID string
	Signer     *attestation.Signer
	// RFC 7517 key set for the attestation public key
	JWKS []byte
	// BootstrapAPIKey is set whenever an initial API key was minted because no
	// active one existed. If set, the caller must show it to the operator; the
	// key was also written to BootstrapAPIKeyFile next to the state DB.
	BootstrapAPIKey     string
	BootstrapAPIKeyFile string
}

// Open opens the sqlite state at path (creating if necessary) and ensures the
// instance is fully initialised.
func Open(path string) (*State, error) {
	// _txlock=immediate makes write transactions take the write lock at BEGIN,
	// avoiding lock upgrades that busy_timeout can't wait out.
	dsn := fmt.Sprintf(
		"file:%s"+
			"?_txlock=immediate"+
			"&_pragma=journal_mode(WAL)"+
			"&_pragma=busy_timeout(5000)"+
			"&_pragma=foreign_keys(1)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// A single connection serialises all access, sidestepping sqlite's
	// single-writer locking within this process.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	st := &State{DB: db}
	if st.InstanceID, err = ensureMeta(db, "instance_id", func() (string, error) {
		return uuid.NewString(), nil
	}); err != nil {
		db.Close()
		return nil, err
	}
	key, err := ensureAttestationKey(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	st.Signer = attestation.NewSigner(key, attestationKid, "wispers-hub/"+st.InstanceID)
	if st.JWKS, err = buildJWKS(key.Public().(ed25519.PublicKey)); err != nil {
		db.Close()
		return nil, err
	}
	if st.BootstrapAPIKey, err = ensureAPIKey(db); err != nil {
		db.Close()
		return nil, err
	}
	if st.BootstrapAPIKey != "" {
		// We log the newly minted API key, but it's easy to miss, so we also
		// write the key next to the state DB and tell the user about it.
		st.BootstrapAPIKeyFile = filepath.Join(filepath.Dir(path), "initial-api-key")
		if err := os.WriteFile(st.BootstrapAPIKeyFile, []byte(st.BootstrapAPIKey+"\n"), 0o600); err != nil {
			db.Close()
			return nil, fmt.Errorf("write %s: %w", st.BootstrapAPIKeyFile, err)
		}
	}
	return st, nil
}

func (s *State) Close() error {
	return s.DB.Close()
}

// ensureMeta returns the value stored under key, generating and storing one
// first if absent.
func ensureMeta(db *sql.DB, key string, generate func() (string, error)) (string, error) {
	var value string
	err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if err == nil {
		return value, nil
	}
	if err != sql.ErrNoRows {
		return "", fmt.Errorf("read meta %q: %w", key, err)
	}
	value, err = generate()
	if err != nil {
		return "", fmt.Errorf("generate meta %q: %w", key, err)
	}
	if _, err := db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)`, key, value); err != nil {
		return "", fmt.Errorf("store meta %q: %w", key, err)
	}
	return value, nil
}

// ensureAttestationKey loads the attestation signing key from the state,
// generating a fresh Ed25519 keypair on first use.
func ensureAttestationKey(db *sql.DB) (ed25519.PrivateKey, error) {
	pemStr, err := ensureMeta(db, "attestation_key_pem", func() (string, error) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return "", err
		}
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			return "", err
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
	})
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("no PEM block in stored attestation key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse stored attestation key: %w", err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("stored attestation key is %T, want ed25519.PrivateKey", parsed)
	}
	return key, nil
}

// buildJWKS renders the RFC 7517 key set for the attestation public key. It is
// served unauthenticated, like the hosted /.well-known/jwks.json, so services
// that receive attestation JWTs can verify them.
func buildJWKS(pub ed25519.PublicKey) ([]byte, error) {
	return json.Marshal(map[string]any{
		"keys": []map[string]any{{
			"kty": "OKP",
			"crv": "Ed25519",
			"alg": "EdDSA",
			"use": "sig",
			"kid": attestationKid,
			"x":   base64.RawURLEncoding.EncodeToString(pub),
		}},
	})
}

// ensureAPIKey mints and stores an API key if no active one exists, returning
// the plaintext (empty if one already existed).
func ensureAPIKey(db *sql.DB) (string, error) {
	var n int
	if err := db.QueryRow(`
		SELECT COUNT(*)
		FROM api_keys
		WHERE revoked_at_millis IS NULL`,
	).Scan(&n); err != nil {
		return "", fmt.Errorf("count api keys: %w", err)
	}
	if n > 0 {
		return "", nil
	}
	key, err := apikeys.Generate(apiKeyEnv)
	if err != nil {
		return "", err
	}
	if _, err := db.Exec(`
		INSERT INTO api_keys (id, secret_hash, name, created_at_millis)
		VALUES (?, ?, ?, ?)`,
		key.ID,
		hex.EncodeToString(key.SecretHash[:]),
		"bootstrap",
		time.Now().UnixMilli(),
	); err != nil {
		return "", fmt.Errorf("store api key: %w", err)
	}
	return key.Encoded, nil
}

// Serve runs backend on an in-memory gRPC transport and returns a client for
// it, so standalone and managed mode can use the gRPC interface.
func Serve(backend bepb.BackendServer) (bepb.BackendClient, func(), error) {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	bepb.RegisterBackendServer(srv, backend)
	go func() {
		if err := srv.Serve(lis); err != nil {
			log.Printf("standalone backend server: %v", err)
		}
	}()
	// passthrough resolver: the target is fake, the dialer ignores it.
	conn, err := grpc.NewClient("passthrough:///standalone",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		srv.Stop()
		return nil, nil, fmt.Errorf("grpc.NewClient: %w", err)
	}
	stop := func() {
		conn.Close()
		srv.Stop()
	}
	return bepb.NewBackendClient(conn), stop, nil
}
