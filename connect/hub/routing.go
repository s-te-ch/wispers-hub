// SPDX-FileCopyrightText: 2026 Scheidegger Technology GmbH
// SPDX-License-Identifier: AGPL-3.0-only

package routing

import (
	"context"
	"log"
	"sync"
	"sync/atomic"

	"connect/client/proto/hubpb"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

//-- Connector -----------------------------------------------------------------

type Connector struct {
	nodeNum int32
	stream  hubpb.Hub_StartServingServer
	group   *connectivityGroup
	outCH   chan *hubpb.ServingRequest
	ctx     context.Context
	cancel  func()
	evicted atomic.Bool
}

func newConnector(
	nodeNum int32,
	stream hubpb.Hub_StartServingServer,
	group *connectivityGroup,
) *Connector {
	c := &Connector{
		nodeNum: nodeNum,
		stream:  stream,
		group:   group,
		outCH:   make(chan *hubpb.ServingRequest),
	}
	c.ctx, c.cancel = context.WithCancel(stream.Context())
	return c
}

func (c *Connector) Run() {
	// Send Welcome to unblock client's stream setup
	c.stream.Send(&hubpb.ServingRequest{
		Kind: &hubpb.ServingRequest_Welcome{Welcome: &hubpb.Welcome{}},
	})
	go c.recvLoop()
	go c.sendLoop()
	<-c.ctx.Done()
}

func (c *Connector) Disconnect() {
	c.group.disconnect(c)
}

// Evicted reports whether this connection was terminated because a newer
// connection for the same node replaced it.
func (c *Connector) Evicted() bool {
	return c.evicted.Load()
}

// Close terminates the connection from the server side; Run returns.
func (c *Connector) Close() {
	c.cancel()
}

func (c *Connector) recvLoop() {
	var msg *hubpb.ServingResponse
	var err error
	for err == nil {
		msg, err = c.stream.Recv()
		if err == nil {
			c.group.processResponse(msg)
		}
	}
	log.Printf("Connector.recvLoop for node %d exited with error: %v", c.nodeNum, err)
	c.cancel()
}

func (c *Connector) sendLoop() {
	var err error
	for err == nil {
		select {
		case <-c.ctx.Done():
			return
		case msg := <-c.outCH:
			err = c.stream.Send(msg)
		}
	}
	log.Printf("Connector.sendLoop for node %d exited with error: %v", c.nodeNum, err)
	c.cancel()
}

func (c *Connector) send(req *hubpb.ServingRequest) error {
	select {
	case <-c.ctx.Done():
		return status.Errorf(codes.Unavailable, "node %d disconnected", c.nodeNum)
	case c.outCH <- req:
		return nil
	}
}

//-- Connectivity group --------------------------------------------------------

type connectivityGroup struct {
	id         string
	mutex      sync.Mutex
	connectors map[int32]*Connector
	msgCounter atomic.Int64
	futures    map[int64]*FutureResponse
}

func newConnectivityGroup(id string) *connectivityGroup {
	return &connectivityGroup{
		id:         id,
		connectors: make(map[int32]*Connector),
		futures:    make(map[int64]*FutureResponse),
	}
}

func (g *connectivityGroup) connect(
	nodeNum int32,
	stream hubpb.Hub_StartServingServer,
) (*Connector, error) {
	c := newConnector(nodeNum, stream, g)
	g.mutex.Lock()
	defer g.mutex.Unlock()
	previous, found := g.connectors[nodeNum]
	if found {
		g.clearFutures(previous.nodeNum)
		previous.cancel()
		previous.evicted.Store(true)
	}
	g.connectors[nodeNum] = c
	if found {
		log.Printf(
			"Node %d reconnected to group %s (now %d connectors)",
			nodeNum, g.id, len(g.connectors),
		)
	} else {
		log.Printf(
			"Node %d connected to group %s (now %d connectors)",
			nodeNum, g.id, len(g.connectors),
		)
	}
	return c, nil
}

func (g *connectivityGroup) disconnect(c *Connector) {
	nodeNum := c.nodeNum
	g.mutex.Lock()
	defer g.mutex.Unlock()
	if current := g.connectors[nodeNum]; c != current {
		// There's another connector under this number now. This happens when a
		// node reconnects after a connection goes bad. Don't touch it.
		return
	}
	delete(g.connectors, nodeNum)
	g.clearFutures(nodeNum)
	log.Printf(
		"Node %d disconnected from group %s (now %d connectors)",
		nodeNum, g.id, len(g.connectors),
	)
}

// clearFutures removes the futures for the given node.
// Must be called under lock!
func (g *connectivityGroup) clearFutures(nodeNum int32) {
	for reqID, f := range g.futures {
		if f.nodeNum == nodeNum {
			delete(g.futures, reqID)
			f.fail(status.Errorf(codes.Unavailable, "node %d disconnected", nodeNum))
		}
	}
}

func (g *connectivityGroup) routeRequest(req *hubpb.ServingRequest) *FutureResponse {
	nodeNum := req.GetDestNodeNumber()
	req.RequestId = g.msgCounter.Add(1)

	g.mutex.Lock()
	c, found := g.connectors[nodeNum]
	connectedNodes := make([]int32, 0, len(g.connectors))
	for n := range g.connectors {
		connectedNodes = append(connectedNodes, n)
	}
	g.mutex.Unlock()

	f := newFutureResponse(nodeNum)
	if !found {
		log.Printf("routeRequest: Node %d not found in group %s, connected nodes: %v", nodeNum, g.id, connectedNodes)
		f.fail(status.Errorf(codes.Unavailable, "node %d is not connected", nodeNum))
	} else if err := c.send(req); err != nil {
		f.fail(err)
	} else {
		g.mutex.Lock()
		g.futures[req.RequestId] = f
		g.mutex.Unlock()
	}
	return f
}

func (g *connectivityGroup) processResponse(resp *hubpb.ServingResponse) {
	if isCheckInMsg(resp) {
		// CheckIn messages are a special case. They're responses to the
		// Welcome message from the hub, not to a message from a peer node.
		return
	}
	reqID := resp.GetRequestId()
	g.mutex.Lock()
	defer g.mutex.Unlock()
	f, found := g.futures[reqID]
	if found {
		delete(g.futures, reqID)
		f.resolve(resp)
	} else {
		log.Printf("Received response for unknown/expired request ID %d", reqID)
	}
}

func isCheckInMsg(resp *hubpb.ServingResponse) bool {
	switch resp.GetKind().(type) {
	case *hubpb.ServingResponse_CheckIn:
		return true
	default:
		return false
	}
}

//-- Router --------------------------------------------------------------------

type Router struct {
	mutex  sync.Mutex
	groups map[string]*connectivityGroup
}

func NewRouter() *Router {
	return &Router{
		groups: make(map[string]*connectivityGroup),
	}
}

func (r *Router) Connect(
	cgID string,
	nodeNum int32,
	stream hubpb.Hub_StartServingServer,
) (*Connector, error) {
	g := r.findOrCreateGroup(cgID)
	return g.connect(nodeNum, stream)
}

func (r *Router) IsNodeConnected(cgID string, nodeNum int32) bool {
	r.mutex.Lock()
	g, found := r.groups[cgID]
	r.mutex.Unlock()
	if !found {
		return false
	}
	g.mutex.Lock()
	_, connected := g.connectors[nodeNum]
	g.mutex.Unlock()
	return connected
}

// ConnectedNodes returns the numbers of the group's currently connected nodes.
func (r *Router) ConnectedNodes(cgID string) []int32 {
	r.mutex.Lock()
	g, found := r.groups[cgID]
	r.mutex.Unlock()
	if !found {
		return nil
	}
	g.mutex.Lock()
	defer g.mutex.Unlock()
	nodes := make([]int32, 0, len(g.connectors))
	for n := range g.connectors {
		nodes = append(nodes, n)
	}
	return nodes
}

func (r *Router) RouteRequest(cgID string, req *hubpb.ServingRequest) *FutureResponse {
	log.Printf("RouteRequest: routing to node %d in group %s", req.GetDestNodeNumber(), cgID)
	g := r.findOrCreateGroup(cgID)
	return g.routeRequest(req)
}

func (r *Router) findOrCreateGroup(cgID string) *connectivityGroup {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	g, found := r.groups[cgID]
	if !found {
		g = newConnectivityGroup(cgID)
		r.groups[cgID] = g
	}
	return g
}

//-- FutureResponse ------------------------------------------------------------

type FutureResponse struct {
	nodeNum int32
	resp    *hubpb.ServingResponse
	err     error
	doneCH  chan struct{}
}

func newFutureResponse(nodeNum int32) *FutureResponse {
	return &FutureResponse{
		nodeNum: nodeNum,
		doneCH:  make(chan struct{}),
	}
}

func (fr *FutureResponse) Await(ctx context.Context) (*hubpb.ServingResponse, error) {
	select {
	case <-fr.doneCH:
		return fr.resp, fr.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (fr *FutureResponse) resolve(resp *hubpb.ServingResponse) {
	fr.resp = resp
	close(fr.doneCH)
}

func (fr *FutureResponse) fail(err error) {
	fr.err = err
	close(fr.doneCH)
}
