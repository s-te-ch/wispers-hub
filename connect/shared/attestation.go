// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

// JWT attestation signing for node registration.
package attestation

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Signer signs attestation JWTs using an Ed25519 private key.
type Signer struct {
	key    ed25519.PrivateKey
	kid    string
	issuer string
}

// NewSigner wraps an in-memory Ed25519 private key in a Signer.
// The kid is embedded in each JWT header for JWKS key selection.
// The issuer is used as the "iss" claim in signed JWTs.
func NewSigner(key ed25519.PrivateKey, kid, issuer string) *Signer {
	return &Signer{key: key, kid: kid, issuer: issuer}
}

// LoadSigner reads an Ed25519 PEM private key and returns a Signer.
func LoadSigner(privateKeyPath, kid, issuer string) (*Signer, error) {
	data, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in %s", privateKeyPath)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key is %T, want ed25519.PrivateKey", parsed)
	}
	return NewSigner(key, kid, issuer), nil
}

// Sign produces a JWT attesting that nodeNumber belongs to cgID.
func (s *Signer) Sign(cgID string, nodeNumber int32) (string, error) {
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"iss":         s.issuer,
		"sub":         cgID,
		"node_number": nodeNumber,
		"iat":         now.Unix(),
	})
	token.Header["kid"] = s.kid
	return token.SignedString(s.key)
}

// LoadJWKS reads the JWKS JSON file and returns its contents.
func LoadJWKS(path string) ([]byte, error) {
	return os.ReadFile(path)
}
