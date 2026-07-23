// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

// Admin REST API for the standalone hub, authenticated by API keys. Paths and
// JSON shapes mirror the hosted integrator API so wcadm and waserver work
// against either backend with only a base-URL change.
package standalone

import (
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"connect/shared/apikeys"
	"connect/shared/integratorapi"

	"connect/client/proto/rosterpb"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	sqlite "modernc.org/sqlite"
)

// OnlineChecker reports which nodes of a connectivity group are currently
// connected. Unlike the (sharded) hosted hub, the standalone hub can answer
// this authoritatively, so this callback is sufficient.
type OnlineChecker func(groupID string) []int32

// NewRESTHandler returns the standalone hub's /api/v1 handler.
func NewRESTHandler(st *State, online OnlineChecker) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())

	// Public JWKS endpoint (no auth required), like the hosted one.
	engine.GET("/.well-known/jwks.json", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", st.JWKS)
	})

	r := &restAPI{db: st.DB, online: online}
	g := engine.Group("/api/v1")
	g.Use(r.checkAPIKey)
	g.GET("/stats", r.getStats)
	g.GET("/connectivity-groups", r.getConnectivityGroups)
	g.POST("/connectivity-groups", r.postConnectivityGroup)
	g.GET("/connectivity-groups/:id", r.getConnectivityGroup)
	g.DELETE("/connectivity-groups/:id", r.deleteConnectivityGroup)
	g.POST("/connectivity-groups/:id/reset", r.resetConnectivityGroup)
	g.POST("/connectivity-groups/:id/registration-tokens", r.postRegistrationToken)
	g.GET("/connectivity-groups/:id/registration-tokens", r.getRegistrationTokens)
	g.PATCH("/connectivity-groups/:id/nodes/:nodeNumber", r.patchNode)
	g.DELETE("/connectivity-groups/:id/nodes/:nodeNumber", r.deleteNode)
	return engine
}

type restAPI struct {
	db     *sql.DB
	online OnlineChecker
}

func (r *restAPI) checkAPIKey(c *gin.Context) {
	apiKey, err := apikeys.FromAuthHeader(c.GetHeader("Authorization"))
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		c.Abort()
		return
	}

	var storedHash string
	var revokedAt sql.NullInt64
	err = r.db.QueryRowContext(
		c.Request.Context(),
		`SELECT secret_hash, revoked_at_millis FROM api_keys WHERE id = ?`,
		apiKey.ID,
	).Scan(&storedHash, &revokedAt)
	if err != nil && err != sql.ErrNoRows {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		c.Abort()
		return
	}
	// One rejection path for unknown id, bad secret, and revoked key.
	providedHash := hex.EncodeToString(apiKey.SecretHash[:])
	if err == sql.ErrNoRows || revokedAt.Valid ||
		subtle.ConstantTimeCompare([]byte(providedHash), []byte(storedHash)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unknown API key"})
		c.Abort()
		return
	}

	c.Set("apiKeyID", apiKey.ID)
	c.Next()
}

func (r *restAPI) getStats(c *gin.Context) {
	var count int32
	err := r.db.QueryRowContext(
		c.Request.Context(),
		`SELECT COUNT(*) FROM connectivity_groups WHERE deleted_at_millis IS NULL`,
	).Scan(&count)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, integratorapi.Stats{
		ConnectivityGroups: integratorapi.GroupsStats{
			Count: count,
			Max:   nil, // no plans in standalone mode: unlimited
		},
	})
}

func (r *restAPI) getConnectivityGroups(c *gin.Context) {
	ctx := c.Request.Context()

	query := `
		SELECT id, name FROM connectivity_groups
		WHERE deleted_at_millis IS NULL
		ORDER BY created_at_millis
	`
	args := []any{}
	if ak := c.Query("associationKey"); ak != "" {
		query = `
			SELECT id, name FROM connectivity_groups
			WHERE deleted_at_millis IS NULL
			AND association_key = ?
		`
		args = append(args, ak)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	result := make([]integratorapi.GroupSummary, 0)
	for rows.Next() {
		var id string
		var name sql.NullString
		if err := rows.Scan(&id, &name); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		result = append(result, integratorapi.GroupSummary{ID: id, Name: nullableStr(name)})
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

func (r *restAPI) postConnectivityGroup(c *gin.Context) {
	var dto integratorapi.CreateConnectivityGroupRequest
	if err := c.ShouldBindJSON(&dto); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	id := uuid.NewString()
	createdAt := time.Now().UnixMilli()
	_, err := r.db.ExecContext(
		c.Request.Context(), `
		INSERT INTO connectivity_groups (id, name, association_key, created_at_millis)
		VALUES (?, ?, ?, ?)`,
		id, toNullString(dto.Name), toNullString(dto.AssociationKey), createdAt)
	if isUniqueViolation(err) {
		c.JSON(http.StatusConflict, gin.H{"error": "association key conflict"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, integratorapi.GroupCreated{
		ID:        id,
		CreatedAt: rfc3339(createdAt),
		Name:      dto.Name,
	})
}

func (r *restAPI) getConnectivityGroup(c *gin.Context) {
	cgID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.AbortWithError(400, err)
		return
	}
	ctx := c.Request.Context()

	var createdAt int64
	var name sql.NullString
	err = r.db.QueryRowContext(
		ctx, `
		SELECT created_at_millis, name FROM connectivity_groups
		WHERE id = ? AND deleted_at_millis IS NULL`,
		cgID.String(),
	).Scan(&createdAt, &name)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Connection state comes straight from the in-process hub.
	onlineNodes := make(map[int32]bool)
	for _, n := range r.online(cgID.String()) {
		onlineNodes[n] = true
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT node_number, name, last_seen_at_millis, metadata, created_at_millis FROM nodes
		WHERE connectivity_group_id = ? AND deleted_at_millis IS NULL
		ORDER BY node_number`, cgID.String())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	nodeList := make([]integratorapi.Node, 0)
	for rows.Next() {
		var nodeNumber int32
		var nodeName, metadata sql.NullString
		var lastSeen sql.NullInt64
		var nodeCreated int64
		if err := rows.Scan(
			&nodeNumber,
			&nodeName,
			&lastSeen,
			&metadata,
			&nodeCreated,
		); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		node := integratorapi.Node{
			NodeNumber: nodeNumber,
			CreatedAt:  rfc3339(nodeCreated),
			Name:       nullableStr(nodeName),
			Metadata:   nullableStr(metadata),
		}
		if lastSeen.Valid {
			ls := rfc3339(lastSeen.Int64)
			node.LastSeenAt = &ls
		}
		// Unlike hosted, always set: there is no "unknown" state here.
		isOnline := onlineNodes[nodeNumber]
		node.IsOnline = &isOnline
		nodeList = append(nodeList, node)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Activity stats (non-fatal on error).
	var successfulConnections7d int32
	weekAgo := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()
	if err := r.db.QueryRowContext(
		ctx, `
		SELECT COUNT(*)
		FROM node_activity_log
		WHERE connectivity_group_id = ?
		AND event_type = 'start-connection'
		AND success = 1
		AND created_at_millis >= ?`,
		cgID.String(),
		weekAgo,
	).Scan(&successfulConnections7d); err != nil {
		log.Printf("CountActivity error: %v", err)
	}

	c.JSON(http.StatusOK, integratorapi.GroupDetail{
		ID:        cgID.String(),
		CreatedAt: rfc3339(createdAt),
		Name:      nullableStr(name),
		Nodes:     nodeList,
		ActivityStats: integratorapi.ActivityStats{
			SuccessfulConnections7d: successfulConnections7d,
		},
	})
}

func (r *restAPI) deleteConnectivityGroup(c *gin.Context) {
	cgID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.AbortWithError(400, err)
		return
	}
	ctx := c.Request.Context()
	now := time.Now().UnixMilli()

	// Cascade the soft-delete to the group's nodes.
	if _, err := r.db.ExecContext(
		ctx, `
		UPDATE nodes
		SET deleted_at_millis = ?
		WHERE connectivity_group_id = ?
		 AND deleted_at_millis IS NULL`,
		now,
		cgID.String(),
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if _, err := r.db.ExecContext(
		ctx, `
		UPDATE connectivity_groups SET deleted_at_millis = ?
		WHERE id = ? AND deleted_at_millis IS NULL`,
		now,
		cgID.String(),
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, integratorapi.GroupDeleted{Deleted: true})
}

func (r *restAPI) resetConnectivityGroup(c *gin.Context) {
	cgID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.AbortWithError(400, err)
		return
	}
	ctx := c.Request.Context()

	if !r.groupExists(c, cgID.String()) {
		return
	}

	// Soft-delete all nodes.
	if _, err := r.db.ExecContext(
		ctx, `
		UPDATE nodes
		SET deleted_at_millis = ?
		WHERE connectivity_group_id = ?
		AND deleted_at_millis IS NULL`,
		time.Now().UnixMilli(),
		cgID.String(),
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Reset the roster.
	if _, err := r.db.ExecContext(ctx, `
		UPDATE connectivity_groups
		SET roster = NULL, roster_version = 0
		WHERE id = ?
		AND deleted_at_millis IS NULL`,
		cgID.String(),
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, integratorapi.GroupReset{Reset: true})
}

func (r *restAPI) postRegistrationToken(c *gin.Context) {
	cgID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid connectivity group ID"})
		return
	}
	ctx := c.Request.Context()

	var dto integratorapi.CreateRegistrationTokenRequest
	if err := c.ShouldBindJSON(&dto); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if !r.groupExists(c, cgID.String()) {
		return
	}

	minted, err := integratorapi.MintRegistrationToken(&dto)
	var ve integratorapi.ValidationError
	if errors.As(err, &ve) {
		c.JSON(http.StatusBadRequest, gin.H{"error": ve.Error()})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if _, err := r.db.ExecContext(
		ctx, `
		INSERT INTO node_registration_tokens (
			id,
			connectivity_group_id,
			token_hash,
			node_name,
			node_metadata,
			created_at_millis,
			expires_at_millis
		)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), cgID.String(), minted.TokenHash,
		toNullString(dto.NodeName), toNullString(dto.NodeMetadata),
		time.Now().UnixMilli(), minted.ExpiresAt.UnixMilli(),
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, integratorapi.RegistrationTokenCreated{
		Token:     minted.Token,
		ExpiresAt: minted.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (r *restAPI) getRegistrationTokens(c *gin.Context) {
	cgID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid connectivity group ID"})
		return
	}
	if !r.groupExists(c, cgID.String()) {
		return
	}

	now := time.Now()
	rows, err := r.db.QueryContext(
		c.Request.Context(), `
		SELECT node_name, node_metadata, created_at_millis, expires_at_millis, used_at_millis
		FROM node_registration_tokens
		WHERE connectivity_group_id = ?
		AND (
			(used_at_millis IS NULL AND expires_at_millis > ?)
			OR created_at_millis >= ?
		)
		ORDER BY created_at_millis DESC`,
		cgID.String(),
		now.UnixMilli(),
		now.Add(-integratorapi.RegistrationTokenListWindow).UnixMilli(),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	result := make([]integratorapi.RegistrationTokenInfo, 0)
	for rows.Next() {
		var nodeName, nodeMetadata sql.NullString
		var createdAt, expiresAt int64
		var usedAt sql.NullInt64
		if err := rows.Scan(&nodeName, &nodeMetadata, &createdAt, &expiresAt, &usedAt); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		info := integratorapi.RegistrationTokenInfo{
			NodeName:     nullableStr(nodeName),
			NodeMetadata: nullableStr(nodeMetadata),
			CreatedAt:    rfc3339(createdAt),
			ExpiresAt:    rfc3339(expiresAt),
		}
		if usedAt.Valid {
			ua := rfc3339(usedAt.Int64)
			info.UsedAt = &ua
		}
		result = append(result, info)
	}
	if err := rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

func (r *restAPI) patchNode(c *gin.Context) {
	cgID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid connectivity group ID"})
		return
	}
	nn, err := strconv.ParseInt(c.Param("nodeNumber"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid node number"})
		return
	}

	var dto integratorapi.PatchNodeRequest
	if err := c.ShouldBindJSON(&dto); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if dto.Name != nil {
		if len(*dto.Name) > 256 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name exceeds 256 characters"})
			return
		}
		if _, err := r.db.ExecContext(
			c.Request.Context(), `
			UPDATE nodes
			SET name = ?
			WHERE connectivity_group_id = ?
			AND node_number = ?
			AND deleted_at_millis IS NULL`,
			*dto.Name, cgID.String(), int32(nn),
		); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{})
}

// deleteNode soft-deletes a single node registration. The node must be revoked
// in the group's roster, because otherwise we get nodes in an illegal
// deregistered-but-still-activated state. Since the hub can't actually verify
// the roster, this is purely defense-in-depth; the caller should make sure
// that the _verified_ roster agrees.
func (r *restAPI) deleteNode(c *gin.Context) {
	cgID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid connectivity group ID"})
		return
	}
	nn, err := strconv.ParseInt(c.Param("nodeNumber"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid node number"})
		return
	}
	ctx := c.Request.Context()

	var rosterBytes []byte
	err = r.db.QueryRowContext(ctx,
		`SELECT roster FROM connectivity_groups WHERE id = ? AND deleted_at_millis IS NULL`,
		cgID.String(),
	).Scan(&rosterBytes)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var one int
	err = r.db.QueryRowContext(ctx, `
		SELECT 1 FROM nodes
		WHERE connectivity_group_id = ?
		AND node_number = ?
		AND deleted_at_millis IS NULL`,
		cgID.String(), int32(nn),
	).Scan(&one)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if !nodeRevokedInRoster(rosterBytes, int32(nn)) {
		c.JSON(http.StatusConflict, gin.H{
			"error": "node is not marked revoked in the group's roster",
		})
		return
	}

	res, err := r.db.ExecContext(ctx, `
		UPDATE nodes
		SET deleted_at_millis = ?
		WHERE connectivity_group_id = ?
		AND node_number = ?
		AND deleted_at_millis IS NULL`,
		time.Now().UnixMilli(), cgID.String(), int32(nn),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, integratorapi.NodeDeleted{Deleted: true})
}

// nodeRevokedInRoster reports whether the given node is revoked in the roster.
func nodeRevokedInRoster(rosterBytes []byte, nodeNumber int32) bool {
	var roster rosterpb.Roster
	if err := proto.Unmarshal(rosterBytes, &roster); err != nil {
		return false
	}
	for _, n := range roster.GetNodes() {
		if n.GetNodeNumber() == nodeNumber && n.GetRevoked() {
			return true
		}
	}
	return false
}

// groupExists checks that the connectivity group is live, writing a 404 (or
// 500) response and returning false if not.
func (r *restAPI) groupExists(c *gin.Context, cgID string) bool {
	var one int
	err := r.db.QueryRowContext(c.Request.Context(),
		`SELECT 1 FROM connectivity_groups WHERE id = ? AND deleted_at_millis IS NULL`, cgID,
	).Scan(&one)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return false
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return false
	}
	return true
}

func toNullString(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

func nullableStr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	return &ns.String
}

func rfc3339(millis int64) string {
	return time.UnixMilli(millis).UTC().Format(time.RFC3339)
}

func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	// 2067 = SQLITE_CONSTRAINT_UNIQUE
	return errors.As(err, &se) && se.Code() == 2067
}
