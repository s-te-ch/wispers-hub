// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

// Standalone-mode implementation of the bepb.Backend interface over sqlite.
// Status codes and token hashing match the hosted backend so clients can't
// tell the difference.
package standalone

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"log"
	"time"

	"connect/proto/bepb"
	"connect/shared/attestation"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Global rate limit on CompleteNodeRegistration (mirrors hubapi).
const (
	registrationRateLimitRPS   rate.Limit = 50
	registrationRateLimitBurst            = 100
)

type Backend struct {
	bepb.UnimplementedBackendServer
	db         *sql.DB
	instanceID string
	signer     *attestation.Signer
	regLimiter *rate.Limiter
}

func NewBackend(st *State) *Backend {
	return &Backend{
		db:         st.DB,
		instanceID: st.InstanceID,
		signer:     st.Signer,
		regLimiter: rate.NewLimiter(registrationRateLimitRPS, registrationRateLimitBurst),
	}
}

func (b *Backend) CompleteNodeRegistration(
	ctx context.Context, req *bepb.CompleteNodeRegistrationRequest,
) (*bepb.CompleteNodeRegistrationResponse, error) {
	// Global DoS guard: protects the DB from registration floods.
	if !b.regLimiter.Allow() {
		err := status.Error(
			codes.ResourceExhausted,
			"registration rate limit exceeded, try again shortly",
		)
		return nil, err
	}

	// The submitted blob is either plaintext T or `codehalf | T` when bound.
	// Don't log the blob: it's a one-shot credential and may carry the codehalf.
	submittedHash := hashToken(req.GetToken())
	log.Printf("CompleteNodeRegistration: hash_prefix=%s", submittedHash[:8])

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, internal("BeginTx", err)
	}
	defer tx.Rollback()

	// Resolve the one-shot registration token to its connectivity group.
	var tokenID, cgID string
	var nodeName, nodeMetadata sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT nrt.id, nrt.connectivity_group_id, nrt.node_name, nrt.node_metadata
		FROM node_registration_tokens nrt
		JOIN connectivity_groups cg ON cg.id = nrt.connectivity_group_id
		WHERE nrt.token_hash = ?
		AND nrt.used_at_millis IS NULL
		AND nrt.expires_at_millis > ?
		AND cg.deleted_at_millis IS NULL`,
		submittedHash, time.Now().UnixMilli(),
	).Scan(&tokenID, &cgID, &nodeName, &nodeMetadata)
	if err == sql.ErrNoRows {
		return nil, status.Error(codes.NotFound, "invalid or expired token")
	} else if err != nil {
		return nil, internal("LookupRegistrationToken", err)
	}

	// Generate auth token.
	authTokenBytes := make([]byte, 32)
	if _, err := rand.Read(authTokenBytes); err != nil {
		return nil, internal("rand.Read", err)
	}
	authToken := hex.EncodeToString(authTokenBytes)

	// Create the node, atomically assigning the next node number, and consume
	// the token — all in one transaction.
	var nodeNumber int32
	err = tx.QueryRowContext(ctx, `
		UPDATE connectivity_groups SET next_node_number = next_node_number + 1
		WHERE id = ? RETURNING next_node_number - 1`, cgID).Scan(&nodeNumber)
	if err != nil {
		return nil, internal("assign node number", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO nodes (
			connectivity_group_id,
			node_number,
			name,
			auth_token_hash,
			metadata,
			created_at_millis
		)
		VALUES (?, ?, ?, ?, ?, ?)`,
		cgID, nodeNumber, nodeName, hashToken(authToken), nodeMetadata, time.Now().UnixMilli(),
	); err != nil {
		return nil, internal("CreateNode", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE node_registration_tokens SET used_at_millis = ? WHERE id = ?`,
		time.Now().UnixMilli(), tokenID,
	); err != nil {
		return nil, internal("MarkNodeRegistrationTokenUsed", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, internal("CompleteNodeRegistration commit", err)
	}

	// Log registration activity (fire-and-forget).
	b.insertActivityLog(ctx, cgID, nodeNumber, "register", true, &bepb.EventDetails{
		Kind: &bepb.EventDetails_Register{Register: &bepb.RegisterEvent{}},
	})

	// Sign attestation JWT (best-effort — registration succeeds even if signing fails).
	var attestationJwt string
	if b.signer != nil {
		jwt, err := b.signer.Sign(cgID, nodeNumber)
		if err != nil {
			log.Printf("Sign attestation JWT error: %v", err)
		} else {
			attestationJwt = jwt
		}
	}

	return &bepb.CompleteNodeRegistrationResponse{
		ConnectivityGroupId: cgID,
		NodeNumber:          nodeNumber,
		AuthToken:           authToken,
		AttestationJwt:      attestationJwt,
	}, nil
}

func (b *Backend) AuthenticateNode(
	ctx context.Context, req *bepb.AuthenticateNodeRequest,
) (*bepb.AuthenticateNodeResponse, error) {
	log.Printf("AuthenticateNode: cg=%s node=%d", req.GetConnectivityGroupId(), req.GetNodeNumber())

	cgID, err := parseGroupID(req.GetConnectivityGroupId())
	if err != nil {
		return nil, err
	}

	var storedHash string
	err = b.db.QueryRowContext(ctx, `
		SELECT auth_token_hash FROM nodes
		WHERE connectivity_group_id = ?
		AND node_number = ?
		AND deleted_at_millis IS NULL`,
		cgID, req.GetNodeNumber(),
	).Scan(&storedHash)
	if err == sql.ErrNoRows {
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	} else if err != nil {
		return nil, internal("GetNode", err)
	}

	providedHash := hashToken(req.GetAuthToken())
	if subtle.ConstantTimeCompare([]byte(providedHash), []byte(storedHash)) != 1 {
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}

	return &bepb.AuthenticateNodeResponse{}, nil
}

func (b *Backend) ListNodes(
	ctx context.Context, req *bepb.ListNodesRequest,
) (*bepb.ListNodesResponse, error) {
	log.Printf("ListNodes: cg=%s", req.GetConnectivityGroupId())

	cgID, err := parseGroupID(req.GetConnectivityGroupId())
	if err != nil {
		return nil, err
	}

	rows, err := b.db.QueryContext(ctx, `
		SELECT node_number, name, last_seen_at_millis, metadata
		FROM nodes
		WHERE connectivity_group_id = ?
		AND deleted_at_millis IS NULL
		ORDER BY node_number`, cgID)
	if err != nil {
		return nil, internal("ListNodes", err)
	}
	defer rows.Close()

	var protoNodes []*bepb.Node
	for rows.Next() {
		var nodeNumber int32
		var name, metadata sql.NullString
		var lastSeen sql.NullInt64
		if err := rows.Scan(&nodeNumber, &name, &lastSeen, &metadata); err != nil {
			return nil, internal("ListNodes scan", err)
		}
		pn := &bepb.Node{NodeNumber: nodeNumber}
		if name.Valid {
			pn.Name = name.String
		}
		if lastSeen.Valid {
			pn.LastSeenAtMillis = lastSeen.Int64
		}
		if metadata.Valid {
			pn.Metadata = metadata.String
		}
		protoNodes = append(protoNodes, pn)
	}
	if err := rows.Err(); err != nil {
		return nil, internal("ListNodes rows", err)
	}

	return &bepb.ListNodesResponse{Nodes: protoNodes}, nil
}

func (b *Backend) GetRoster(
	ctx context.Context, req *bepb.GetRosterRequest,
) (*bepb.GetRosterResponse, error) {
	log.Printf("GetRoster: cg=%s", req.GetConnectivityGroupId())

	cgID, err := parseGroupID(req.GetConnectivityGroupId())
	if err != nil {
		return nil, err
	}

	var roster []byte
	var version int64
	err = b.db.QueryRowContext(ctx, `
		SELECT roster, roster_version
		FROM connectivity_groups
		WHERE id = ?
		AND deleted_at_millis IS NULL`,
		cgID,
	).Scan(&roster, &version)
	if err == sql.ErrNoRows {
		return nil, status.Error(codes.NotFound, "connectivity group not found")
	} else if err != nil {
		return nil, internal("GetRoster", err)
	}

	return &bepb.GetRosterResponse{Roster: roster, Version: version}, nil
}

func (b *Backend) GetGroupMetadata(
	ctx context.Context, req *bepb.GetGroupMetadataRequest,
) (*bepb.GetGroupMetadataResponse, error) {
	log.Printf("GetGroupMetadata: cg=%s", req.GetConnectivityGroupId())

	cgID, err := parseGroupID(req.GetConnectivityGroupId())
	if err != nil {
		return nil, err
	}

	var createdAt int64
	var name, associationKey sql.NullString
	err = b.db.QueryRowContext(ctx, `
		SELECT created_at_millis, name, association_key
		FROM connectivity_groups
		WHERE id = ?
		AND deleted_at_millis IS NULL`,
		cgID,
	).Scan(&createdAt, &name, &associationKey)
	if err == sql.ErrNoRows {
		return nil, status.Error(codes.NotFound, "connectivity group not found")
	} else if err != nil {
		return nil, internal("GetGroupMetadata", err)
	}

	// A standalone hub has no domains; the instance ID stands in so the field
	// is still a stable identifier for "whoever runs this group".
	resp := &bepb.GetGroupMetadataResponse{
		DomainId:        b.instanceID,
		CreatedAtMillis: createdAt,
	}
	if name.Valid {
		resp.Name = name.String
	}
	if associationKey.Valid {
		resp.AssociationKey = associationKey.String
	}
	return resp, nil
}

func (b *Backend) UpdateRoster(
	ctx context.Context, req *bepb.UpdateRosterRequest,
) (*bepb.UpdateRosterResponse, error) {
	log.Printf("UpdateRoster: cg=%s new_version=%d expected_version=%d",
		req.GetConnectivityGroupId(), req.GetNewVersion(), req.GetExpectedVersion())

	cgID, err := parseGroupID(req.GetConnectivityGroupId())
	if err != nil {
		return nil, err
	}

	if req.GetNewVersion() <= req.GetExpectedVersion() {
		err := status.Error(
			codes.InvalidArgument,
			"new_version must be greater than expected_version",
		)
		return nil, err
	}

	// Atomic optimistic-locking update: zero rows affected means version
	// mismatch (or missing group), same as the hosted RETURNING+ErrNoRows.
	res, err := b.db.ExecContext(ctx, `
		UPDATE connectivity_groups
		SET roster = ?,
			roster_version = ?
		WHERE id = ?
		AND roster_version = ?
		AND deleted_at_millis IS NULL`,
		req.GetRoster(),
		req.GetNewVersion(),
		cgID,
		req.GetExpectedVersion(),
	)
	if err != nil {
		return nil, internal("UpdateRoster", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, internal("UpdateRoster rows affected", err)
	} else if n == 0 {
		return nil, status.Error(codes.Aborted, "roster version conflict")
	}

	return &bepb.UpdateRosterResponse{}, nil
}

func (b *Backend) UpdateNodeLastSeen(
	ctx context.Context, req *bepb.UpdateNodeLastSeenRequest,
) (*bepb.UpdateNodeLastSeenResponse, error) {
	cgID, err := parseGroupID(req.GetConnectivityGroupId())
	if err != nil {
		return nil, err
	}

	if _, err := b.db.ExecContext(ctx, `
		UPDATE nodes
		SET last_seen_at_millis = ?
		WHERE connectivity_group_id = ?
		AND node_number = ?`,
		time.Now().UnixMilli(),
		cgID,
		req.GetNodeNumber(),
	); err != nil {
		return nil, internal("UpdateNodeLastSeen", err)
	}

	return &bepb.UpdateNodeLastSeenResponse{}, nil
}

func (b *Backend) DeregisterNode(
	ctx context.Context, req *bepb.DeregisterNodeRequest,
) (*bepb.DeregisterNodeResponse, error) {
	log.Printf("DeregisterNode: cg=%s node=%d", req.GetConnectivityGroupId(), req.GetNodeNumber())

	cgID, err := parseGroupID(req.GetConnectivityGroupId())
	if err != nil {
		return nil, err
	}

	if _, err := b.db.ExecContext(ctx, `
		UPDATE nodes
		SET deleted_at_millis = ?
		WHERE connectivity_group_id = ?
		AND node_number = ?`,
		time.Now().UnixMilli(),
		cgID,
		req.GetNodeNumber(),
	); err != nil {
		return nil, internal("DeleteNode", err)
	}

	// Log deregistration activity (fire-and-forget).
	b.insertActivityLog(ctx, cgID, req.GetNodeNumber(), "deregister", true, &bepb.EventDetails{
		Kind: &bepb.EventDetails_Deregister{Deregister: &bepb.DeregisterEvent{}},
	})

	return &bepb.DeregisterNodeResponse{}, nil
}

func (b *Backend) LogActivity(
	ctx context.Context, req *bepb.LogActivityRequest,
) (*bepb.LogActivityResponse, error) {
	cgID, err := parseGroupID(req.GetConnectivityGroupId())
	if err != nil {
		return nil, err
	}

	b.insertActivityLog(
		ctx,
		cgID,
		req.GetNodeNumber(),
		req.GetEventType(),
		req.GetSuccess(),
		req.GetDetails(),
	)
	return &bepb.LogActivityResponse{}, nil
}

// insertActivityLog is a best-effort helper: any error is logged but never
// fails the operation it annotates.
func (b *Backend) insertActivityLog(
	ctx context.Context,
	cgID string,
	nodeNumber int32,
	eventType string,
	success bool,
	details *bepb.EventDetails,
) {
	var detailsBytes []byte
	if details != nil {
		var err error
		detailsBytes, err = proto.Marshal(details)
		if err != nil {
			log.Printf("insertActivityLog: marshal details error: %v", err)
			return
		}
	}

	if _, err := b.db.ExecContext(ctx, `
		INSERT INTO node_activity_log (
			connectivity_group_id,
			node_number,
			event_type,
			success,
			details,
			created_at_millis
		)
		VALUES (?, ?, ?, ?, ?, ?)`,
		cgID, nodeNumber, eventType, success, detailsBytes, time.Now().UnixMilli(),
	); err != nil {
		log.Printf("insertActivityLog error: %v", err)
	}
}

// parseGroupID validates the request's connectivity group ID, returning the
// canonical string form used as the sqlite key.
func parseGroupID(raw string) (string, error) {
	cgID, err := uuid.Parse(raw)
	if err != nil {
		return "", status.Error(codes.InvalidArgument, "invalid connectivity_group_id")
	}
	return cgID.String(), nil
}

// internal logs the underlying error and returns an opaque Internal status,
// mirroring hubapi's convention.
func internal(op string, err error) error {
	log.Printf("%s error: %v", op, err)
	return status.Error(codes.Internal, "internal error")
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
