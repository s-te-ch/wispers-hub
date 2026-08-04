// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

package hubsrv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"

	"connect/client/proto/hubpb"
	"connect/client/proto/rosterpb"
	"connect/hub/routing"
	"connect/proto/bepb"
	"connect/shared/turncreds"

	"golang.org/x/time/rate"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Limiter provides per-key rate limiting.
type Limiter struct {
	limiters sync.Map // map[string]*rate.Limiter
	rate     rate.Limit
	burst    int
}

// NewLimiter creates a rate limiter with the specified rate (requests per second) and burst size.
func NewLimiter(rps float64, burst int) *Limiter {
	return &Limiter{
		rate:  rate.Limit(rps),
		burst: burst,
	}
}

// Allow checks if a request for the given key should be allowed.
func (l *Limiter) Allow(key string) bool {
	val, _ := l.limiters.LoadOrStore(key, rate.NewLimiter(l.rate, l.burst))
	return val.(*rate.Limiter).Allow()
}

type HubServer struct {
	hubpb.UnimplementedHubServer
	beClient   bepb.BackendClient
	router     *routing.Router
	limiter    *Limiter
	stunServer string
	turnServer string // empty disables TURN
	turnSecret []byte
}

func NewHubServer(beClient bepb.BackendClient, stunServer, turnServer string, turnSecret []byte) *HubServer {
	return &HubServer{
		beClient:   beClient,
		router:     routing.NewRouter(),
		limiter:    NewLimiter(2.0, 20), // 2 QPS, burst of 20
		stunServer: stunServer,
		turnServer: turnServer,
		turnSecret: turnSecret,
	}
}

// ConnectedNodes returns the numbers of the group's nodes that are currently
// connected to this hub.
func (s *HubServer) ConnectedNodes(cgID string) []int32 {
	return s.router.ConnectedNodes(cgID)
}

func (s *HubServer) CompleteNodeRegistration(
	ctx context.Context, req *hubpb.NodeRegistrationRequest,
) (*hubpb.NodeRegistration, error) {
	// Don't log the token: it's a one-shot registration credential (and may
	// carry the codehalf). Log only a hash prefix, which correlates with the
	// backend's CompleteNodeRegistration log line.
	tokenHash := sha256.Sum256([]byte(req.GetToken()))
	log.Printf("CompleteNodeRegistration received: hash_prefix=%s", hex.EncodeToString(tokenHash[:])[:8])

	beResp, err := s.beClient.CompleteNodeRegistration(ctx, &bepb.CompleteNodeRegistrationRequest{
		Token: req.GetToken(),
	})
	if err != nil {
		log.Printf("Backend error: %v", err)
		return nil, err // Pass through backend error
	}

	return &hubpb.NodeRegistration{
		ConnectivityGroupId: beResp.GetConnectivityGroupId(),
		NodeNumber:          beResp.GetNodeNumber(),
		AuthToken:           beResp.GetAuthToken(),
		AttestationJwt:      beResp.GetAttestationJwt(),
	}, nil
}

func (s *HubServer) PairNodes(
	ctx context.Context, req *hubpb.PairNodesMessage,
) (*hubpb.PairNodesMessage, error) {
	cgID, nodeNum, err := s.authenticateAndRateLimit(ctx)
	if err != nil {
		return nil, err
	}
	sr := &hubpb.ServingRequest{
		DestNodeNumber:   req.GetPayload().GetReceiverNodeNumber(),
		SourceNodeNumber: nodeNum,
		// RequestId: Set by the router.
		Kind: &hubpb.ServingRequest_PairNodesMessage{
			PairNodesMessage: req,
		},
	}
	future := s.router.RouteRequest(cgID, sr)
	resp, err := future.Await(ctx)
	if err != nil {
		return nil, err
	}
	if resp.GetError() != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "peer rejected request: %s", resp.GetError())
	}

	s.logActivity(ctx, cgID, nodeNum, "activate", true, &bepb.EventDetails{
		Kind: &bepb.EventDetails_Activate{Activate: &bepb.ActivateEvent{
			EndorserNodeNumber: req.GetPayload().GetReceiverNodeNumber(),
		}},
	})
	return resp.GetPairNodesMessage(), nil
}

func (s *HubServer) UpdateRoster(
	ctx context.Context, req *hubpb.UpdateRosterRequest,
) (*hubpb.UpdateRosterResponse, error) {
	cgID, callerNodeNum, err := s.authenticateAndRateLimit(ctx)
	if err != nil {
		return nil, err
	}

	roster := req.GetNewRoster()
	addenda := roster.GetAddenda()
	if len(addenda) == 0 {
		return nil, status.Error(codes.InvalidArgument, "roster has no addenda")
	}
	newAddendum := addenda[len(addenda)-1]

	var expectedVersion int64
	switch kind := newAddendum.GetKind().(type) {
	case *rosterpb.Addendum_Activation:
		activation := kind.Activation
		payload := activation.GetPayload()
		expectedVersion = payload.GetVersion() - 1
		if callerNodeNum != payload.GetNewNodeNumber() {
			return nil, status.Error(codes.PermissionDenied, "caller must be the new node")
		}
		if err := s.getEndorserCosignature(ctx, cgID, callerNodeNum, roster, activation); err != nil {
			return nil, err
		}

	case *rosterpb.Addendum_Revocation:
		payload := kind.Revocation.GetPayload()
		expectedVersion = payload.GetVersion() - 1
		if callerNodeNum != payload.GetRevokerNodeNumber() {
			return nil, status.Error(codes.PermissionDenied, "caller must be the revoker")
		}

	default:
		return nil, status.Error(codes.InvalidArgument, "addendum has no activation or revocation")
	}

	rosterBytes, err := proto.Marshal(roster)
	if err != nil {
		log.Printf("Marshal roster error: %v", err)
		return nil, status.Error(codes.Internal, "failed to serialize roster")
	}

	_, err = s.beClient.UpdateRoster(ctx, &bepb.UpdateRosterRequest{
		ConnectivityGroupId: cgID,
		Roster:              rosterBytes,
		NewVersion:          roster.GetVersion(),
		ExpectedVersion:     expectedVersion,
	})
	if err != nil {
		log.Printf("UpdateRoster backend error: %v", err)
		return nil, err
	}

	// Log roster update activity
	var rosterKind string
	switch newAddendum.GetKind().(type) {
	case *rosterpb.Addendum_Activation:
		rosterKind = "activation"
	case *rosterpb.Addendum_Revocation:
		rosterKind = "revocation"
	}
	s.logActivity(ctx, cgID, callerNodeNum, "roster_update", true, &bepb.EventDetails{
		Kind: &bepb.EventDetails_RosterUpdate{RosterUpdate: &bepb.RosterUpdateEvent{
			Kind: rosterKind,
		}},
	})

	return &hubpb.UpdateRosterResponse{
		CosignedRoster: roster,
	}, nil
}

func (s *HubServer) getEndorserCosignature(
	ctx context.Context,
	cgID string,
	nodeNum int32,
	roster *rosterpb.Roster,
	activation *rosterpb.Activation,
) error {
	payload := activation.GetPayload()
	cosignReq := &hubpb.ServingRequest{
		DestNodeNumber:   payload.GetEndorserNodeNumber(),
		SourceNodeNumber: nodeNum,
		Kind: &hubpb.ServingRequest_RosterCosignRequest{
			RosterCosignRequest: &hubpb.RosterCosignRequest{
				NewNodeNumber: int64(payload.GetNewNodeNumber()),
				NewRoster:     roster,
			},
		},
	}
	future := s.router.RouteRequest(cgID, cosignReq)
	resp, err := future.Await(ctx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "endorser unreachable: %v", err)
	}
	if resp.GetError() != "" {
		return status.Errorf(codes.FailedPrecondition, "endorser rejected: %s", resp.GetError())
	}

	cosignResp := resp.GetRosterCosignResponse()
	if cosignResp == nil {
		return status.Error(codes.Internal, "invalid cosign response")
	}
	activation.EndorserSignature = cosignResp.GetEndorserSignature()
	return nil
}

func (s *HubServer) ListNodes(
	ctx context.Context, req *hubpb.ListNodesRequest,
) (*hubpb.NodeList, error) {
	cgID, _, err := s.authenticateAndRateLimit(ctx)
	if err != nil {
		return nil, err
	}

	beResp, err := s.beClient.ListNodes(ctx, &bepb.ListNodesRequest{
		ConnectivityGroupId: cgID,
	})
	if err != nil {
		log.Printf("Backend error: %v", err)
		return nil, err
	}

	nodes := make([]*hubpb.Node, 0, len(beResp.GetNodes()))
	for _, n := range beResp.GetNodes() {
		nodes = append(nodes, &hubpb.Node{
			NodeNumber:       n.GetNodeNumber(),
			Name:             n.GetName(),
			LastSeenAtMillis: n.GetLastSeenAtMillis(),
			Metadata:         n.GetMetadata(),
			IsOnline:         s.router.IsNodeConnected(cgID, n.GetNodeNumber()),
		})
	}

	return &hubpb.NodeList{Nodes: nodes}, nil
}

func (s *HubServer) GetRoster(
	ctx context.Context, req *hubpb.RosterRequest,
) (*rosterpb.Roster, error) {
	cgID, _, err := s.authenticateAndRateLimit(ctx)
	if err != nil {
		return nil, err
	}

	beResp, err := s.beClient.GetRoster(ctx, &bepb.GetRosterRequest{
		ConnectivityGroupId: cgID,
	})
	if err != nil {
		log.Printf("GetRoster backend error: %v", err)
		return nil, err
	}

	// Empty roster is valid (no nodes activated yet)
	if len(beResp.GetRoster()) == 0 {
		return &rosterpb.Roster{}, nil
	}

	// Deserialize
	roster := &rosterpb.Roster{}
	if err := proto.Unmarshal(beResp.GetRoster(), roster); err != nil {
		log.Printf("Unmarshal roster error: %v", err)
		return nil, status.Error(codes.Internal, "failed to deserialize roster")
	}

	return roster, nil
}

func (s *HubServer) GetGroupMetadata(
	ctx context.Context, req *hubpb.GroupMetadataRequest,
) (*hubpb.GroupMetadata, error) {
	cgID, _, err := s.authenticateAndRateLimit(ctx)
	if err != nil {
		return nil, err
	}
	beResp, err := s.beClient.GetGroupMetadata(ctx, &bepb.GetGroupMetadataRequest{
		ConnectivityGroupId: cgID,
	})
	if err != nil {
		log.Printf("GetGroupMetadata backend error: %v", err)
		return nil, err
	}
	return &hubpb.GroupMetadata{
		Id:              cgID,
		Name:            beResp.Name,
		CreatedAtMillis: beResp.CreatedAtMillis,
	}, nil
}

// turnCredentialTTL covers connection setup. Established connections
// survive it.
const turnCredentialTTL = 10 * time.Minute

func (s *HubServer) GetStunTurnConfig(
	ctx context.Context, req *hubpb.StunTurnConfigRequest,
) (*hubpb.StunTurnConfig, error) {
	cgID, _, err := s.authenticateAndRateLimit(ctx)
	if err != nil {
		return nil, err
	}

	cfg := &hubpb.StunTurnConfig{StunServer: s.stunServer}
	if s.turnServer == "" {
		return cfg, nil
	}
	policy, err := s.beClient.GetTurnPolicy(ctx, &bepb.GetTurnPolicyRequest{
		ConnectivityGroupId: cgID,
	})
	if err != nil {
		log.Printf("GetTurnPolicy(%s) backend error: %v", cgID, err)
		return nil, err // Pass through backend error
	}
	if !policy.GetEnabled() {
		return cfg, nil // plan without TURN
	}

	// Truncate to whole seconds. The credential's wire format carries Unix
	// seconds, and ExpiresAtMillis must agree with it exactly.
	expiry := time.Now().Add(turnCredentialTTL).Truncate(time.Second)
	username, password := turncreds.Mint(
		s.turnSecret,
		"org-"+policy.GetOrganisationId(),
		policy.GetFloorBps(),
		policy.GetCeilBps(),
		expiry,
	)
	cfg.TurnServer = s.turnServer
	cfg.TurnUsername = username
	cfg.TurnPassword = password
	cfg.ExpiresAtMillis = expiry.UnixMilli()
	return cfg, nil
}

func (s *HubServer) StartConnection(
	ctx context.Context, req *hubpb.StartConnectionRequest,
) (*hubpb.StartConnectionResponse, error) {
	cgID, nodeNum, err := s.authenticateAndRateLimit(ctx)
	if err != nil {
		return nil, err
	}

	// Decode the signed payload to read routing and logging fields.
	// The raw signed_payload bytes are forwarded unchanged to the
	// answerer so the caller's signature remains valid.
	var payload hubpb.StartConnectionRequest_Payload
	if err := proto.Unmarshal(req.GetSignedPayload(), &payload); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "cannot decode signed payload: %v", err)
	}

	sr := &hubpb.ServingRequest{
		DestNodeNumber:   payload.GetAnswererNodeNumber(),
		SourceNodeNumber: nodeNum,
		Kind: &hubpb.ServingRequest_StartConnectionRequest{
			StartConnectionRequest: req,
		},
	}
	transport := payload.GetTransport().String()

	future := s.router.RouteRequest(cgID, sr)
	resp, err := future.Await(ctx)
	if err != nil {
		s.logActivity(ctx, cgID, nodeNum, "start-connection", false, &bepb.EventDetails{
			ErrorMessage: err.Error(),
			Kind: &bepb.EventDetails_Connection{Connection: &bepb.ConnectionEvent{
				Transport:      transport,
				PeerNodeNumber: payload.GetAnswererNodeNumber(),
			}},
		})
		return nil, err
	}
	if resp.GetError() != "" {
		s.logActivity(ctx, cgID, nodeNum, "start-connection", false, &bepb.EventDetails{
			ErrorMessage: resp.GetError(),
			Kind: &bepb.EventDetails_Connection{Connection: &bepb.ConnectionEvent{
				Transport:      transport,
				PeerNodeNumber: payload.GetAnswererNodeNumber(),
			}},
		})
		return nil, status.Errorf(codes.FailedPrecondition, "peer rejected connection: %s", resp.GetError())
	}

	s.logActivity(ctx, cgID, nodeNum, "start-connection", true, &bepb.EventDetails{
		Kind: &bepb.EventDetails_Connection{Connection: &bepb.ConnectionEvent{
			Transport:      transport,
			PeerNodeNumber: payload.GetAnswererNodeNumber(),
		}},
	})
	return resp.GetStartConnectionResponse(), nil
}

func (s *HubServer) DeregisterNode(
	ctx context.Context, req *hubpb.DeregisterNodeRequest,
) (*hubpb.DeregisterNodeResponse, error) {
	cgID, nodeNum, err := s.authenticateAndRateLimit(ctx)
	if err != nil {
		return nil, err
	}

	_, err = s.beClient.DeregisterNode(ctx, &bepb.DeregisterNodeRequest{
		ConnectivityGroupId: cgID,
		NodeNumber:          nodeNum,
	})
	if err != nil {
		log.Printf("DeregisterNode backend error: %v", err)
		return nil, err
	}

	return &hubpb.DeregisterNodeResponse{}, nil
}

func (s *HubServer) StartServing(stream hubpb.Hub_StartServingServer) error {
	cgID, nodeNum, err := s.authenticateAndRateLimit(stream.Context())
	if err != nil {
		return err
	}

	// Update last_seen_at timestamp
	_, err = s.beClient.UpdateNodeLastSeen(stream.Context(), &bepb.UpdateNodeLastSeenRequest{
		ConnectivityGroupId: cgID,
		NodeNumber:          nodeNum,
	})
	if err != nil {
		log.Printf("UpdateNodeLastSeen failed for node %d in %s: %v", nodeNum, cgID, err)
		// Don't fail the connection for this
	}

	s.logActivity(stream.Context(), cgID, nodeNum, "connect", true, &bepb.EventDetails{
		Kind: &bepb.EventDetails_Connect{Connect: &bepb.ConnectEvent{}},
	})

	c, err := s.router.Connect(cgID, nodeNum, stream)
	if err != nil {
		return err
	}
	defer c.Disconnect()
	c.Run()

	s.logActivity(context.Background(), cgID, nodeNum, "disconnect", true, &bepb.EventDetails{
		Kind: &bepb.EventDetails_Disconnect{Disconnect: &bepb.DisconnectEvent{}},
	})
	// Tell the evicted client why its stream ended, so a duplicate instance shows
	// up clearly in its own logs instead of looking like a random disconnect.
	if c.Evicted() {
		return status.Error(
			codes.Aborted,
			"connection superseded by a newer connection for this node; "+
				"is another instance running on the same credentials?",
		)
	}
	return nil
}

// logActivity sends a fire-and-forget activity log event to the backend.
func (s *HubServer) logActivity(ctx context.Context, cgID string, nodeNum int32, eventType string, success bool, details *bepb.EventDetails) {
	_, err := s.beClient.LogActivity(ctx, &bepb.LogActivityRequest{
		ConnectivityGroupId: cgID,
		NodeNumber:          nodeNum,
		EventType:           eventType,
		Success:             success,
		Details:             details,
	})
	if err != nil {
		log.Printf("logActivity %s error: %v", eventType, err)
	}
}

// authenticateAndRateLimit checks rate limit (using claimed identity), then authenticates.
// Rate limiting before auth protects the database from unauthenticated spam.
// Returns (connectivity_group_id, node_number, error).
func (s *HubServer) authenticateAndRateLimit(ctx context.Context) (string, int32, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", 0, status.Error(codes.Unauthenticated, "missing metadata")
	}

	cgID := getMetadataValue(md, "x-connectivity-group-id")
	nodeNumStr := getMetadataValue(md, "x-node-number")
	authToken := getMetadataValue(md, "x-auth-token")

	if cgID == "" || nodeNumStr == "" || authToken == "" {
		return "", 0, status.Error(codes.Unauthenticated, "missing credentials")
	}

	var nodeNum int32
	if _, err := fmt.Sscanf(nodeNumStr, "%d", &nodeNum); err != nil {
		return "", 0, status.Error(codes.Unauthenticated, "invalid node number")
	}

	// Rate limit per claimed node identity (before DB lookup).
	key := fmt.Sprintf("%s:%d", cgID, nodeNum)
	if !s.limiter.Allow(key) {
		return "", 0, status.Error(codes.ResourceExhausted, "rate limit exceeded")
	}

	// Validate with backend (requires DB lookup).
	_, err := s.beClient.AuthenticateNode(ctx, &bepb.AuthenticateNodeRequest{
		ConnectivityGroupId: cgID,
		NodeNumber:          nodeNum,
		AuthToken:           authToken,
	})
	if err != nil {
		return "", 0, err
	}

	return cgID, nodeNum, nil
}

func getMetadataValue(md metadata.MD, key string) string {
	vals := md.Get(key)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}
