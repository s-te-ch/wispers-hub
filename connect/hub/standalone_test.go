// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

package standalone_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"connect/hub/standalone"
	"connect/proto/bepb"

	"connect/client/proto/rosterpb"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// doJSON performs a request against the REST handler and decodes the JSON
// response into out (pass nil to skip decoding).
func doJSON(t *testing.T, h http.Handler, method, path, apiKey string, body any, out any) int {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if out != nil && w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("decode response %q: %v", w.Body.String(), err)
		}
	}
	return w.Code
}

func TestStandaloneEndToEnd(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "hub.db")

	st, err := standalone.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if st.BootstrapAPIKey == "" {
		t.Fatal("expected a bootstrap API key on first open")
	}
	keyFile, err := os.ReadFile(st.BootstrapAPIKeyFile)
	if err != nil {
		t.Fatalf("read bootstrap key file: %v", err)
	}
	if strings.TrimSpace(string(keyFile)) != st.BootstrapAPIKey {
		t.Error("bootstrap key file does not match the minted key")
	}
	if fi, err := os.Stat(st.BootstrapAPIKeyFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("bootstrap key file: mode %v err %v, want 0600", fi.Mode().Perm(), err)
	}
	if st.InstanceID == "" {
		t.Fatal("expected an instance ID")
	}
	adminKey := st.BootstrapAPIKey

	online := map[string][]int32{}
	rest := standalone.NewRESTHandler(st, func(groupID string) []int32 { return online[groupID] })
	client, stop, err := standalone.Serve(standalone.NewBackend(st))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}

	// --- REST auth ---
	if code := doJSON(t, rest, "GET", "/api/v1/stats", "", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("no key: got %d, want 401", code)
	}
	if code := doJSON(
		t, rest, "GET", "/api/v1/stats", "wc_standalone_garbage.nope", nil, nil,
	); code != http.StatusUnauthorized {
		t.Errorf("garbage key: got %d, want 401", code)
	}
	if code := doJSON(
		t, rest, "GET", "/api/v1/stats", adminKey, nil, nil,
	); code != http.StatusOK {
		t.Fatalf("valid key: got %d, want 200", code)
	}

	// --- Group lifecycle over REST ---
	var group struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if code := doJSON(t, rest, "POST", "/api/v1/connectivity-groups",
		adminKey, map[string]any{"name": "test-group"}, &group); code != http.StatusOK {
		t.Fatalf("create group: got %d, want 200", code)
	}
	if group.ID == "" || group.Name != "test-group" {
		t.Fatalf("create group: unexpected response %+v", group)
	}

	var groups []struct {
		ID string `json:"id"`
	}
	if code := doJSON(
		t, rest, "GET", "/api/v1/connectivity-groups", adminKey, nil, &groups,
	); code != http.StatusOK {
		t.Fatalf("list groups: got %d, want 200", code)
	}
	if len(groups) != 1 || groups[0].ID != group.ID {
		t.Fatalf("list groups: got %+v, want the created group", groups)
	}

	// --- Registration token + node registration over the backend interface ---
	var tokenResp struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
	}
	if code := doJSON(
		t, rest, "POST", "/api/v1/connectivity-groups/"+group.ID+"/registration-tokens",
		adminKey, map[string]any{"nodeName": "laptop"}, &tokenResp,
	); code != http.StatusOK {
		t.Fatalf("create token: got %d, want 200", code)
	}
	if tokenResp.Token == "" || tokenResp.ExpiresAt == "" {
		t.Fatalf("create token: unexpected response %+v", tokenResp)
	}

	// Node quota in the group detail: no plans in standalone mode, so the
	// limit is null, but current still counts nodes + pending tokens.
	var detail struct {
		Nodes     []json.RawMessage `json:"nodes"`
		NodeQuota struct {
			Limit   *int32 `json:"limit"`
			Current int32  `json:"current"`
		} `json:"nodeQuota"`
	}
	getDetail := func() {
		t.Helper()
		if code := doJSON(
			t, rest, "GET", "/api/v1/connectivity-groups/"+group.ID, adminKey, nil, &detail,
		); code != http.StatusOK {
			t.Fatalf("get group: got %d, want 200", code)
		}
		if detail.NodeQuota.Limit != nil {
			t.Errorf("node quota limit: got %v, want null (unlimited)", *detail.NodeQuota.Limit)
		}
	}
	getDetail()
	if detail.NodeQuota.Current != 1 || len(detail.Nodes) != 0 {
		t.Errorf("pending token quota: got current %d with %d nodes, want 1 and 0",
			detail.NodeQuota.Current, len(detail.Nodes))
	}

	reg, err := client.CompleteNodeRegistration(ctx, &bepb.CompleteNodeRegistrationRequest{
		Token: tokenResp.Token,
	})
	if err != nil {
		t.Fatalf("CompleteNodeRegistration: %v", err)
	}
	unexpected := reg.GetConnectivityGroupId() != group.ID ||
		reg.GetNodeNumber() != 1 ||
		reg.GetAuthToken() == ""
	if unexpected {
		t.Fatalf("CompleteNodeRegistration: unexpected response %+v", reg)
	}
	if reg.GetAttestationJwt() == "" {
		t.Error("expected an attestation JWT")
	}

	// Redeeming the token turned its pending quota usage into a node.
	getDetail()
	if detail.NodeQuota.Current != 1 || len(detail.Nodes) != 1 {
		t.Errorf("post-registration quota: got current %d with %d nodes, want 1 and 1",
			detail.NodeQuota.Current, len(detail.Nodes))
	}

	// The JWKS is served without auth and verifies the attestation JWT.
	var jwks struct {
		Keys []struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			Kid string `json:"kid"`
			X   string `json:"x"`
		} `json:"keys"`
	}
	if code := doJSON(
		t, rest, "GET", "/.well-known/jwks.json", "", nil, &jwks,
	); code != http.StatusOK {
		t.Fatalf("jwks: got %d, want 200", code)
	}
	if len(jwks.Keys) != 1 || jwks.Keys[0].Kty != "OKP" || jwks.Keys[0].Crv != "Ed25519" {
		t.Fatalf("jwks: unexpected key set %+v", jwks.Keys)
	}
	pub, err := base64.RawURLEncoding.DecodeString(jwks.Keys[0].X)
	if err != nil {
		t.Fatalf("jwks: decode x: %v", err)
	}
	jwtParts := strings.Split(reg.GetAttestationJwt(), ".")
	if len(jwtParts) != 3 {
		t.Fatalf("attestation JWT: got %d parts, want 3", len(jwtParts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(jwtParts[2])
	if err != nil {
		t.Fatalf("attestation JWT: decode signature: %v", err)
	}
	if !ed25519.Verify(pub, []byte(jwtParts[0]+"."+jwtParts[1]), sig) {
		t.Error("attestation JWT does not verify against the published JWKS")
	}

	// The token is one-shot.
	_, err = client.CompleteNodeRegistration(ctx, &bepb.CompleteNodeRegistrationRequest{
		Token: tokenResp.Token,
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("token reuse: got %v, want NotFound", err)
	}

	// --- Node auth ---
	_, err = client.AuthenticateNode(ctx, &bepb.AuthenticateNodeRequest{
		ConnectivityGroupId: group.ID,
		NodeNumber:          reg.GetNodeNumber(),
		AuthToken:           reg.GetAuthToken(),
	})
	if err != nil {
		t.Fatalf("AuthenticateNode: %v", err)
	}
	_, err = client.AuthenticateNode(ctx, &bepb.AuthenticateNodeRequest{
		ConnectivityGroupId: group.ID,
		NodeNumber:          reg.GetNodeNumber(),
		AuthToken:           "wrong",
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("bad auth token: got %v, want Unauthenticated", err)
	}

	// --- Group metadata ---
	meta, err := client.GetGroupMetadata(ctx, &bepb.GetGroupMetadataRequest{
		ConnectivityGroupId: group.ID,
	})
	if err != nil {
		t.Fatalf("GetGroupMetadata: %v", err)
	}
	if meta.GetDomainId() != st.InstanceID {
		t.Errorf("domain_id: got %q, want instance ID %q", meta.GetDomainId(), st.InstanceID)
	}
	if meta.GetName() != "test-group" {
		t.Errorf("name: got %q, want test-group", meta.GetName())
	}

	// --- Roster with optimistic locking ---
	roster, err := client.GetRoster(ctx, &bepb.GetRosterRequest{ConnectivityGroupId: group.ID})
	if err != nil {
		t.Fatalf("GetRoster: %v", err)
	}
	if roster.GetVersion() != 0 || len(roster.GetRoster()) != 0 {
		t.Fatalf(
			"fresh roster: got version=%d len=%d, want 0/0",
			roster.GetVersion(),
			len(roster.GetRoster()),
		)
	}
	if _, err := client.UpdateRoster(ctx, &bepb.UpdateRosterRequest{
		ConnectivityGroupId: group.ID,
		Roster:              []byte("fake-roster"),
		NewVersion:          1,
		ExpectedVersion:     0,
	}); err != nil {
		t.Fatalf("UpdateRoster: %v", err)
	}
	_, err = client.UpdateRoster(ctx, &bepb.UpdateRosterRequest{
		ConnectivityGroupId: group.ID,
		Roster:              []byte("stale"),
		NewVersion:          1,
		ExpectedVersion:     0,
	})
	if status.Code(err) != codes.Aborted {
		t.Errorf("stale roster update: got %v, want Aborted", err)
	}
	roster, err = client.GetRoster(ctx, &bepb.GetRosterRequest{ConnectivityGroupId: group.ID})
	if err != nil {
		t.Fatalf("GetRoster after update: %v", err)
	}
	if roster.GetVersion() != 1 || string(roster.GetRoster()) != "fake-roster" {
		t.Fatalf("updated roster: got version=%d roster=%q", roster.GetVersion(), roster.GetRoster())
	}

	// --- Node listing, last-seen, REST view ---
	if _, err := client.UpdateNodeLastSeen(ctx, &bepb.UpdateNodeLastSeenRequest{
		ConnectivityGroupId: group.ID,
		NodeNumber:          reg.GetNodeNumber(),
	}); err != nil {
		t.Fatalf("UpdateNodeLastSeen: %v", err)
	}
	nodes, err := client.ListNodes(ctx, &bepb.ListNodesRequest{ConnectivityGroupId: group.ID})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes.GetNodes()) != 1 || nodes.GetNodes()[0].GetName() != "laptop" ||
		nodes.GetNodes()[0].GetLastSeenAtMillis() == 0 {
		t.Fatalf("ListNodes: unexpected %+v", nodes.GetNodes())
	}

	online[group.ID] = []int32{reg.GetNodeNumber()}
	var groupDetail struct {
		Nodes []struct {
			NodeNumber int32  `json:"nodeNumber"`
			Name       string `json:"name"`
			LastSeenAt string `json:"lastSeenAt"`
			IsOnline   *bool  `json:"isOnline"`
		} `json:"nodes"`
	}
	if code := doJSON(t, rest, "GET", "/api/v1/connectivity-groups/"+group.ID,
		adminKey, nil, &groupDetail); code != http.StatusOK {
		t.Fatalf("get group: got %d, want 200", code)
	}
	if len(groupDetail.Nodes) != 1 || groupDetail.Nodes[0].Name != "laptop" ||
		groupDetail.Nodes[0].LastSeenAt == "" ||
		groupDetail.Nodes[0].IsOnline == nil || !*groupDetail.Nodes[0].IsOnline {
		t.Fatalf("get group: unexpected nodes %+v", groupDetail.Nodes)
	}

	// --- Deregistration ---
	if _, err := client.DeregisterNode(ctx, &bepb.DeregisterNodeRequest{
		ConnectivityGroupId: group.ID,
		NodeNumber:          reg.GetNodeNumber(),
	}); err != nil {
		t.Fatalf("DeregisterNode: %v", err)
	}
	nodes, err = client.ListNodes(ctx, &bepb.ListNodesRequest{ConnectivityGroupId: group.ID})
	if err != nil {
		t.Fatalf("ListNodes after deregister: %v", err)
	}
	if len(nodes.GetNodes()) != 0 {
		t.Fatalf("ListNodes after deregister: got %d nodes, want 0", len(nodes.GetNodes()))
	}

	// --- Reopen: no re-mint while an active key exists ---
	stop()
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st2, err := standalone.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	if st2.BootstrapAPIKey != "" {
		t.Error("reopen minted a new API key despite an active one existing")
	}
	if st2.InstanceID != st.InstanceID {
		t.Errorf("instance ID changed across reopen: %q → %q", st.InstanceID, st2.InstanceID)
	}
}

// Coverage for GET /connectivity-groups/:id/registration-tokens: pending
// tokens always list, recent used/expired ones list as history, old expired
// ones drop out, and the token secret never leaves the server.
func TestRegistrationTokenListing(t *testing.T) {
	st, err := standalone.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	adminKey := st.BootstrapAPIKey
	rest := standalone.NewRESTHandler(st, func(string) []int32 { return nil })

	var group struct {
		ID string `json:"id"`
	}
	if code := doJSON(t, rest, "POST", "/api/v1/connectivity-groups",
		adminKey, map[string]any{"name": "tokens"}, &group); code != http.StatusOK {
		t.Fatalf("create group: got %d, want 200", code)
	}
	listPath := "/api/v1/connectivity-groups/" + group.ID + "/registration-tokens"

	// Unknown group: 404.
	if code := doJSON(t, rest, "GET",
		"/api/v1/connectivity-groups/00000000-0000-0000-0000-000000000000/registration-tokens",
		adminKey, nil, nil); code != http.StatusNotFound {
		t.Errorf("unknown group: got %d, want 404", code)
	}

	// No tokens yet: an empty array, not null.
	var listing []map[string]any
	if code := doJSON(t, rest, "GET", listPath, adminKey, nil, &listing); code != http.StatusOK {
		t.Fatalf("empty list: got %d, want 200", code)
	}
	if listing == nil || len(listing) != 0 {
		t.Errorf("empty list: got %v, want []", listing)
	}

	// Mint three tokens, then push them into distinct states directly in the
	// DB (the REST API deliberately has no way to do this). Explicit created
	// times make the newest-first order assertable.
	mint := func(nodeName string) string {
		var resp struct {
			Token string `json:"token"`
		}
		if code := doJSON(t, rest, "POST", listPath, adminKey,
			map[string]any{"nodeName": nodeName}, &resp); code != http.StatusOK {
			t.Fatalf("mint %s: got %d, want 200", nodeName, code)
		}
		return resp.Token
	}
	pendingToken := mint("pending")
	mint("used-recently")
	mint("expired-long-ago")

	now := time.Now()
	setTimes := func(nodeName string, createdAt, expiresAt, usedAt any) {
		if _, err := st.DB.Exec(`
			UPDATE node_registration_tokens
			SET created_at_millis = ?, expires_at_millis = ?, used_at_millis = ?
			WHERE node_name = ?`,
			createdAt, expiresAt, usedAt, nodeName,
		); err != nil {
			t.Fatalf("set times of %s: %v", nodeName, err)
		}
	}
	eightDaysAgo := now.Add(-8 * 24 * time.Hour).UnixMilli()
	inAnHour := now.Add(time.Hour).UnixMilli()
	setTimes("pending", now.Add(-2*time.Minute).UnixMilli(), inAnHour, nil)
	setTimes("used-recently", now.Add(-time.Minute).UnixMilli(), inAnHour, now.UnixMilli())
	setTimes("expired-long-ago", eightDaysAgo, eightDaysAgo, nil)

	req := httptest.NewRequest("GET", listPath, nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	w := httptest.NewRecorder()
	rest.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: got %d, want 200", w.Code)
	}
	// No token material in the response, under any key.
	if strings.Contains(w.Body.String(), pendingToken) ||
		strings.Contains(w.Body.String(), "token\"") ||
		strings.Contains(w.Body.String(), "tokenHash") {
		t.Errorf("listing leaks token material: %s", w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	if len(listing) != 2 {
		t.Fatalf("got %d entries, want 2 (the old expired one dropped): %v", len(listing), listing)
	}
	if name := listing[0]["nodeName"]; name != "used-recently" {
		t.Errorf("entry 0: got %v, want the newest (used-recently)", name)
	}
	if listing[0]["usedAt"] == nil {
		t.Error("used token: usedAt should be set")
	}
	if name := listing[1]["nodeName"]; name != "pending" {
		t.Errorf("entry 1: got %v, want pending", name)
	}
	if usedAt, present := listing[1]["usedAt"]; !present || usedAt != nil {
		t.Errorf("pending token: usedAt should be present and null, got %v (present=%v)",
			usedAt, present)
	}
}

// Node deletion (DELETE /connectivity-groups/:id/nodes/:nodeNumber) frees a
// node's registration, but only once the group's roster marks the node
// revoked — the precondition that keeps registrations out of the
// deregistered-but-still-activated state. Exercises the guard from both
// sides plus the 404 paths.
func TestStandaloneNodeDelete(t *testing.T) {
	ctx := context.Background()

	st, err := standalone.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	adminKey := st.BootstrapAPIKey

	rest := standalone.NewRESTHandler(st, func(string) []int32 { return nil })
	client, stop, err := standalone.Serve(standalone.NewBackend(st))
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer stop()

	var group struct {
		ID string `json:"id"`
	}
	if code := doJSON(t, rest, "POST", "/api/v1/connectivity-groups",
		adminKey, map[string]any{"name": "delete-test"}, &group); code != http.StatusOK {
		t.Fatalf("create group: got %d, want 200", code)
	}
	var tokenResp struct {
		Token string `json:"token"`
	}
	if code := doJSON(
		t, rest, "POST", "/api/v1/connectivity-groups/"+group.ID+"/registration-tokens",
		adminKey, map[string]any{"nodeName": "guest"}, &tokenResp,
	); code != http.StatusOK {
		t.Fatalf("create token: got %d, want 200", code)
	}
	reg, err := client.CompleteNodeRegistration(ctx, &bepb.CompleteNodeRegistrationRequest{
		Token: tokenResp.Token,
	})
	if err != nil {
		t.Fatalf("CompleteNodeRegistration: %v", err)
	}
	node := reg.GetNodeNumber()
	nodePath := "/api/v1/connectivity-groups/" + group.ID + "/nodes/" +
		strconv.Itoa(int(node))

	// No roster yet: the precondition cannot hold.
	if code := doJSON(t, rest, "DELETE", nodePath, adminKey, nil, nil); code != http.StatusConflict {
		t.Errorf("delete without roster: got %d, want 409", code)
	}

	// Garbage roster: untrusted input must fail closed.
	setTestRoster(t, ctx, client, group.ID, 1, []byte("not-a-roster"))
	if code := doJSON(t, rest, "DELETE", nodePath, adminKey, nil, nil); code != http.StatusConflict {
		t.Errorf("delete with garbage roster: got %d, want 409", code)
	}

	// Roster lists the node, but not as revoked.
	setTestRoster(t, ctx, client, group.ID, 2, marshalRoster(t, &rosterpb.Roster{
		Version: 2,
		Nodes:   []*rosterpb.Node{{NodeNumber: node}},
	}))
	if code := doJSON(t, rest, "DELETE", nodePath, adminKey, nil, nil); code != http.StatusConflict {
		t.Errorf("delete unrevoked node: got %d, want 409", code)
	}

	// Revoked in the roster: the delete goes through.
	setTestRoster(t, ctx, client, group.ID, 3, marshalRoster(t, &rosterpb.Roster{
		Version: 3,
		Nodes:   []*rosterpb.Node{{NodeNumber: node, Revoked: true}},
	}))
	var deleted struct {
		Deleted bool `json:"deleted"`
	}
	if code := doJSON(t, rest, "DELETE", nodePath, adminKey, nil, &deleted); code != http.StatusOK {
		t.Fatalf("delete revoked node: got %d, want 200", code)
	}
	if !deleted.Deleted {
		t.Error("delete revoked node: expected deleted=true")
	}
	nodes, err := client.ListNodes(ctx, &bepb.ListNodesRequest{ConnectivityGroupId: group.ID})
	if err != nil {
		t.Fatalf("ListNodes after delete: %v", err)
	}
	if len(nodes.GetNodes()) != 0 {
		t.Fatalf("ListNodes after delete: got %d nodes, want 0", len(nodes.GetNodes()))
	}

	// The registration is gone: retries and unknown nodes 404.
	if code := doJSON(t, rest, "DELETE", nodePath, adminKey, nil, nil); code != http.StatusNotFound {
		t.Errorf("second delete: got %d, want 404", code)
	}
	if code := doJSON(t, rest, "DELETE",
		"/api/v1/connectivity-groups/"+group.ID+"/nodes/999",
		adminKey, nil, nil); code != http.StatusNotFound {
		t.Errorf("delete unknown node: got %d, want 404", code)
	}
}

// setTestRoster uploads a roster blob via the backend interface, as the
// sharer node would.
func setTestRoster(
	t *testing.T, ctx context.Context, client bepb.BackendClient,
	groupID string, version int64, blob []byte,
) {
	t.Helper()
	if _, err := client.UpdateRoster(ctx, &bepb.UpdateRosterRequest{
		ConnectivityGroupId: groupID,
		Roster:              blob,
		NewVersion:          version,
		ExpectedVersion:     version - 1,
	}); err != nil {
		t.Fatalf("UpdateRoster v%d: %v", version, err)
	}
}

func marshalRoster(t *testing.T, roster *rosterpb.Roster) []byte {
	t.Helper()
	blob, err := proto.Marshal(roster)
	if err != nil {
		t.Fatalf("marshal roster: %v", err)
	}
	return blob
}
