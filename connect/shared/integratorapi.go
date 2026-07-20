// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

// Wire shapes and shared semantics of the integrator REST API (/api/v1).
//
// Both implementations of the API, the hosted backend and the hub's standalone
// mode, compile against these types so their JSON cannot drift apart silently.
// The shapes are public contract: wcadm and waserver are built against them.
package integratorapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Request bodies /////////////////////////////////////////////////////////////

type CreateConnectivityGroupRequest struct {
	Name           *string `json:"name"`
	AssociationKey *string `json:"associationKey"`
}

type CreateRegistrationTokenRequest struct {
	NodeName     *string `json:"nodeName"`
	NodeMetadata *string `json:"nodeMetadata"`
	// Codehalf, when present, binds the issued token to a client-side secret to
	// defend against deeplink hijack: instead of storing the plaintext token T,
	// we store SHA256(codehalf | T). Finalize must submit `codehalf | T`.
	Codehalf *string `json:"codehalf"`
	// TTLProfile selects the token's entropy and TTL (see
	// registrationTTLProfiles). Optional; defaults to "interactive". Mirrors
	// the client's TtlProfile for activation codes.
	TTLProfile *string `json:"ttlProfile"`
}

type PatchNodeRequest struct {
	Name *string `json:"name"`
}

// Response bodies ////////////////////////////////////////////////////////////

type GroupSummary struct {
	ID   string  `json:"id"`
	Name *string `json:"name,omitempty"`
}

type GroupCreated struct {
	ID        string  `json:"id"`
	CreatedAt string  `json:"createdAt"` // RFC 3339
	Name      *string `json:"name,omitempty"`
}

type GroupDetail struct {
	ID            string        `json:"id"`
	CreatedAt     string        `json:"createdAt"` // RFC 3339
	Name          *string       `json:"name,omitempty"`
	Nodes         []Node        `json:"nodes"` // initialise to render [] instead of null
	ActivityStats ActivityStats `json:"activityStats"`
}

type Node struct {
	NodeNumber int32   `json:"nodeNumber"`
	CreatedAt  string  `json:"createdAt"` // RFC 3339
	Name       *string `json:"name,omitempty"`
	LastSeenAt *string `json:"lastSeenAt,omitempty"` // RFC 3339
	Metadata   *string `json:"metadata,omitempty"`
	IsOnline   *bool   `json:"isOnline,omitempty"` // absent when unknown
}

type ActivityStats struct {
	SuccessfulConnections7d int32 `json:"successfulConnections7d"`
}

type RegistrationTokenCreated struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt"` // RFC 3339
}

// RegistrationTokenInfo is one entry of the registration-token listing
// (`GET /connectivity-groups/:id/registration-tokens`, a bare array).
//
// Returns all pending tokens, plus a 7-day window of used & expired ones.
// Newest first.
type RegistrationTokenInfo struct {
	NodeName     *string `json:"nodeName,omitempty"`
	NodeMetadata *string `json:"nodeMetadata,omitempty"`
	CreatedAt    string  `json:"createdAt"` // RFC 3339
	ExpiresAt    string  `json:"expiresAt"` // RFC 3339
	// Always present, null if unused.
	UsedAt *string `json:"usedAt"` // RFC 3339
}

// RegistrationTokenListWindow is how far back listing historical tokens goes.
const RegistrationTokenListWindow = 7 * 24 * time.Hour

type GroupDeleted struct {
	Deleted bool `json:"deleted"`
}

type GroupReset struct {
	Reset bool `json:"reset"`
}

type Stats struct {
	ConnectivityGroups GroupsStats `json:"connectivityGroups"`
}

type GroupsStats struct {
	Count int32  `json:"count"`
	Max   *int32 `json:"max"` // null = unlimited
}

// QuotaExceeded is the body of 429 responses to quota violations.
type QuotaExceeded struct {
	Error   string `json:"error"` // always "quota exceeded"
	Quota   string `json:"quota"` // e.g. "groups_per_domain", "nodes_per_group"
	Limit   int32  `json:"limit"`
	Current int32  `json:"current"`
}

// Registration token minting /////////////////////////////////////////////////

// ValidationError is a client error; its message is the public HTTP 400
// response body.
type ValidationError string

func (e ValidationError) Error() string { return string(e) }

// Map of public `ttlProfile` names to their token entropy and TTL.
//
//	interactive  — short-lived, minimal-typing codes (Wispers Files): entered
//				   live, at the keyboard.
//	asynchronous — long-lived codes (Wispers Access): generated now and
//				   delivered out-of-band (e.g. email) to be used later.
type registrationTTLProfile struct {
	tokenBytes int
	ttl        time.Duration
}

var registrationTTLProfiles = map[string]registrationTTLProfile{
	"interactive":  {tokenBytes: 4, ttl: 5 * time.Minute},
	"asynchronous": {tokenBytes: 8, ttl: 24 * time.Hour},
}

const defaultRegistrationTTLProfile = "interactive"

// MintedRegistrationToken is the result of MintRegistrationToken. Only the
// hash may be stored; Token is returned to the caller once.
type MintedRegistrationToken struct {
	Token     string
	TokenHash string // hex SHA256 of the token (or codehalf|token)
	ExpiresAt time.Time
}

// MintRegistrationToken validates the request and generates a registration
// token according to its TTL profile. Validation failures are returned as
// ValidationError (HTTP 400); any other error is internal.
func MintRegistrationToken(req *CreateRegistrationTokenRequest) (*MintedRegistrationToken, error) {
	if req.NodeName != nil && len(*req.NodeName) > 256 {
		return nil, ValidationError("nodeName exceeds 256 characters")
	}
	if req.NodeMetadata != nil && len(*req.NodeMetadata) > 256 {
		return nil, ValidationError("nodeMetadata exceeds 256 characters")
	}
	if req.Codehalf != nil && (len(*req.Codehalf) < 16 || len(*req.Codehalf) > 256) {
		return nil, ValidationError("codehalf must be 16-256 characters")
	}

	profileName := defaultRegistrationTTLProfile
	if req.TTLProfile != nil {
		profileName = *req.TTLProfile
	}
	profile, ok := registrationTTLProfiles[profileName]
	if !ok {
		return nil, ValidationError(`ttlProfile must be "interactive" or "asynchronous"`)
	}

	// Generate token T (tokenBytes hex chars from CSPRNG). If a codehalf is
	// given, use that as a hash prefix.
	tokenBytes := make([]byte, profile.tokenBytes)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(tokenBytes)
	hashInput := token
	if req.Codehalf != nil {
		hashInput = *req.Codehalf + token
	}
	h := sha256.Sum256([]byte(hashInput))

	return &MintedRegistrationToken{
		Token:     token,
		TokenHash: hex.EncodeToString(h[:]),
		ExpiresAt: time.Now().Add(profile.ttl),
	}, nil
}
