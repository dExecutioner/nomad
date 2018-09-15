package nomad

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/rpc"
	"strings"
	"time"

	metrics "github.com/armon/go-metrics"
	log "github.com/hashicorp/go-hclog"
	memdb "github.com/hashicorp/go-memdb"

	"github.com/hashicorp/consul/lib"
	"github.com/hashicorp/nomad/helper/pool"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/raft"
	"github.com/hashicorp/yamux"
	"github.com/ugorji/go/codec"
)

const (
	// maxQueryTime is used to bound the limit of a blocking query
	maxQueryTime = 300 * time.Second

	// defaultQueryTime is the amount of time we block waiting for a change
	// if no time is specified. Previously we would wait the maxQueryTime.
	defaultQueryTime = 300 * time.Second

	// Warn if the Raft command is larger than this.
	// If it's over 1MB something is probably being abusive.
	raftWarnSize = 1024 * 1024

	// enqueueLimit caps how long we will wait to enqueue
	// a new Raft command. Something is probably wrong if this
	// value is ever reached. However, it prevents us from blocking
	// the requesting goroutine forever.
	enqueueLimit = 30 * time.Second
)

type rpcHandler struct {
	*Server
	logger log.Logger
}

func newRpcHandler(s *Server) *rpcHandler {
	return &rpcHandler{
		Server: s,
		logger: s.logger.Named("rpc"),
	}
}

// RPCContext provides metadata about the RPC connection.
type RPCContext struct {
	// Conn exposes the raw connection.
	Conn net.Conn

	// Session exposes the multiplexed connection session.
	Session *yamux.Session

	// TLS marks whether the RPC is over a TLS based connection
	TLS bool

	// VerifiedChains is is the Verified certificates presented by the incoming
	// connection.
	VerifiedChains [][]*x509.Certificate

	// NodeID marks the NodeID that initiated the connection.
	NodeID string
}

// listen is used to listen for incoming RPC connections
func (r *rpcHandler) listen(ctx context.Context) {
	defer close(r.listenerCh)
	for {
		select {
		case <-ctx.Done():
			r.logger.Info("closing server RPC connection")
			return
		default:
		}

		// Accept a connection
		conn, err := r.rpcListener.Accept()
		if err != nil {
			if r.shutdown {
				return
			}

			select {
			case <-ctx.Done():
				return
			default:
			}

			r.logger.Error("failed to accept RPC conn", "error", err)
			continue
		}

		go r.handleConn(ctx, conn, &RPCContext{Conn: conn})
		metrics.IncrCounter([]string{"nomad", "rpc", "accept_conn"}, 1)
	}
}

// handleConn is used to determine if this is a Raft or
// Nomad type RPC connection and invoke the correct handler
func (r *rpcHandler) handleConn(ctx context.Context, conn net.Conn, rpcCtx *RPCContext) {
	// Read a single byte
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != nil {
		if err != io.EOF {
			r.logger.Error("failed to read first RPC byte", "error", err)
		}
		conn.Close()
		return
	}

	// Enforce TLS if EnableRPC is set
	if r.config.TLSConfig.EnableRPC && !rpcCtx.TLS && pool.RPCType(buf[0]) != pool.RpcTLS {
		if !r.config.TLSConfig.RPCUpgradeMode {
			r.logger.Warn("non-TLS connection attempted with RequireTLS set", "remote_addr", conn.RemoteAddr())
			conn.Close()
			return
		}
	}

	// Switch on the byte
	switch pool.RPCType(buf[0]) {
	case pool.RpcNomad:
		// Create an RPC Server and handle the request
		server := rpc.NewServer()
		r.setupRpcServer(server, rpcCtx)
		r.handleNomadConn(ctx, conn, server)

		// Remove any potential mapping between a NodeID to this connection and
		// close the underlying connection.
		r.removeNodeConn(rpcCtx)

	case pool.RpcRaft:
		metrics.IncrCounter([]string{"nomad", "rpc", "raft_handoff"}, 1)
		r.raftLayer.Handoff(ctx, conn)

	case pool.RpcMultiplex:
		r.handleMultiplex(ctx, conn, rpcCtx)

	case pool.RpcTLS:
		if r.rpcTLS == nil {
			r.logger.Warn("TLS connection attempted, server not configured for TLS")
			conn.Close()
			return
		}
		conn = tls.Server(conn, r.rpcTLS)

		// Force a handshake so we can get information about the TLS connection
		// state.
		tlsConn, ok := conn.(*tls.Conn)
		if !ok {
			r.logger.Error("expected TLS connection", "got", log.Fmt("%T", conn))
			conn.Close()
			return
		}

		if err := tlsConn.Handshake(); err != nil {
			r.logger.Warn("failed TLS handshake", "remote_addr", tlsConn.RemoteAddr(), "error", err)
			conn.Close()
			return
		}

		// Update the connection context with the fact that the connection is
		// using TLS
		rpcCtx.TLS = true

		// Store the verified chains so they can be inspected later.
		state := tlsConn.ConnectionState()
		rpcCtx.VerifiedChains = state.VerifiedChains

		r.handleConn(ctx, conn, rpcCtx)

	case pool.RpcStreaming:
		r.handleStreamingConn(conn)

	case pool.RpcMultiplexV2:
		r.handleMultiplexV2(ctx, conn, rpcCtx)

	default:
		r.logger.Error("unrecognized RPC byte", "byte", buf[0])
		conn.Close()
		return
	}
}

// handleMultiplex is used to multiplex a single incoming connection
// using the Yamux multiplexer
func (r *rpcHandler) handleMultiplex(ctx context.Context, conn net.Conn, rpcCtx *RPCContext) {
	defer func() {
		// Remove any potential mapping between a NodeID to this connection and
		// close the underlying connection.
		r.removeNodeConn(rpcCtx)
		conn.Close()
	}()

	conf := yamux.DefaultConfig()
	conf.LogOutput = r.config.LogOutput
	server, err := yamux.Server(conn, conf)
	if err != nil {
		r.logger.Error("multiplex failed to create yamux server", "error", err)
		return
	}

	// Update the context to store the yamux session
	rpcCtx.Session = server

	// Create the RPC server for this connection
	rpcServer := rpc.NewServer()
	r.setupRpcServer(rpcServer, rpcCtx)

	for {
		// stop handling connections if context was cancelled
		if ctx.Err() != nil {
			return
		}

		sub, err := server.Accept()
		if err != nil {
			if err != io.EOF {
				r.logger.Error("multiplex conn accept failed", "error", err)
			}
			return
		}
		go r.handleNomadConn(ctx, sub, rpcServer)
	}
}

// handleNomadConn is used to service a single Nomad RPC connection
func (r *rpcHandler) handleNomadConn(ctx context.Context, conn net.Conn, server *rpc.Server) {
	defer conn.Close()
	rpcCodec := pool.NewServerCodec(conn)
	for {
		select {
		case <-ctx.Done():
			r.logger.Info("closing server RPC connection")
			return
		case <-r.shutdownCh:
			return
		default:
		}

		if err := server.ServeRequest(rpcCodec); err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "closed") {
				r.logger.Error("RPC error", "error", err, "connection", conn)
				metrics.IncrCounter([]string{"nomad", "rpc", "request_error"}, 1)
			}
			return
		}
		metrics.IncrCounter([]string{"nomad", "rpc", "request"}, 1)
	}
}

// handleStreamingConn is used to handle a single Streaming Nomad RPC connection.
func (r *rpcHandler) handleStreamingConn(conn net.Conn) {
	defer conn.Close()

	// Decode the header
	var header structs.StreamingRpcHeader
	decoder := codec.NewDecoder(conn, structs.MsgpackHandle)
	if err := decoder.Decode(&header); err != nil {
		if err != io.EOF && !strings.Contains(err.Error(), "closed") {
			r.logger.Error("streaming RPC error", "error", err, "connection", conn)
			metrics.IncrCounter([]string{"nomad", "streaming_rpc", "request_error"}, 1)
		}

		return
	}

	ack := structs.StreamingRpcAck{}
	handler, err := r.streamingRpcs.GetHandler(header.Method)
	if err != nil {
		r.logger.Error("streaming RPC error", "error", err, "connection", conn)
		metrics.IncrCounter([]string{"nomad", "streaming_rpc", "request_error"}, 1)
		ack.Error = err.Error()
	}

	// Send the acknowledgement
	encoder := codec.NewEncoder(conn, structs.MsgpackHandle)
	if err := encoder.Encode(ack); err != nil {
		conn.Close()
		return
	}

	if ack.Error != "" {
		return
	}

	// Invoke the handler
	metrics.IncrCounter([]string{"nomad", "streaming_rpc", "request"}, 1)
	handler(conn)
}

// handleMultiplexV2 is used to multiplex a single incoming connection
// using the Yamux multiplexer. Version 2 handling allows a single connection to
// switch streams between regulars RPCs and Streaming RPCs.
func (r *rpcHandler) handleMultiplexV2(ctx context.Context, conn net.Conn, rpcCtx *RPCContext) {
	defer func() {
		// Remove any potential mapping between a NodeID to this connection and
		// close the underlying connection.
		r.removeNodeConn(rpcCtx)
		conn.Close()
	}()

	conf := yamux.DefaultConfig()
	conf.LogOutput = r.config.LogOutput
	server, err := yamux.Server(conn, conf)
	if err != nil {
		r.logger.Error("multiplex_v2 failed to create yamux server", "error", err)
		return
	}

	// Update the context to store the yamux session
	rpcCtx.Session = server

	// Create the RPC server for this connection
	rpcServer := rpc.NewServer()
	r.setupRpcServer(rpcServer, rpcCtx)

	for {
		// stop handling connections if context was cancelled
		if ctx.Err() != nil {
			return
		}

		// Accept a new stream
		sub, err := server.Accept()
		if err != nil {
			if err != io.EOF {
				r.logger.Error("multiplex_v2 conn accept failed", "error", err)
			}
			return
		}

		// Read a single byte
		buf := make([]byte, 1)
		if _, err := sub.Read(buf); err != nil {
			if err != io.EOF {
				r.logger.Error("multiplex_v2 failed to read first byte", "error", err)
			}
			return
		}

		// Determine which handler to use
		switch pool.RPCType(buf[0]) {
		case pool.RpcNomad:
			go r.handleNomadConn(ctx, sub, rpcServer)
		case pool.RpcStreaming:
			go r.handleStreamingConn(sub)

		default:
			r.logger.Error("multiplex_v2 unrecognized first RPC byte", "byte", buf[0])
			return
		}
	}

}

// forward is used to forward to a remote region or to forward to the local leader
// Returns a bool of if forwarding was performed, as well as any error
func (r *rpcHandler) forward(method string, info structs.RPCInfo, args interface{}, reply interface{}) (bool, error) {
	var firstCheck time.Time

	region := info.RequestRegion()
	if region == "" {
		return true, fmt.Errorf("missing target RPC")
	}

	// Handle region forwarding
	if region != r.config.Region {
		// Mark that we are forwarding the RPC
		info.SetForwarded()
		err := r.forwardRegion(region, method, args, reply)
		return true, err
	}

	// Check if we can allow a stale read
	if info.IsRead() && info.AllowStaleRead() {
		return false, nil
	}

CHECK_LEADER:
	// Find the leader
	isLeader, remoteServer := r.getLeader()

	// Handle the case we are the leader
	if isLeader {
		return false, nil
	}

	// Handle the case of a known leader
	if remoteServer != nil {
		// Mark that we are forwarding the RPC
		info.SetForwarded()
		err := r.forwardLeader(remoteServer, method, args, reply)
		return true, err
	}

	// Gate the request until there is a leader
	if firstCheck.IsZero() {
		firstCheck = time.Now()
	}
	if time.Now().Sub(firstCheck) < r.config.RPCHoldTimeout {
		jitter := lib.RandomStagger(r.config.RPCHoldTimeout / structs.JitterFraction)
		select {
		case <-time.After(jitter):
			goto CHECK_LEADER
		case <-r.shutdownCh:
		}
	}

	// No leader found and hold time exceeded
	return true, structs.ErrNoLeader
}

// getLeader returns if the current node is the leader, and if not
// then it returns the leader which is potentially nil if the cluster
// has not yet elected a leader.
func (s *Server) getLeader() (bool, *serverParts) {
	// Check if we are the leader
	if s.IsLeader() {
		return true, nil
	}

	// Get the leader
	leader := s.raft.Leader()
	if leader == "" {
		return false, nil
	}

	// Lookup the server
	s.peerLock.RLock()
	server := s.localPeers[leader]
	s.peerLock.RUnlock()

	// Server could be nil
	return false, server
}

// forwardLeader is used to forward an RPC call to the leader, or fail if no leader
func (r *rpcHandler) forwardLeader(server *serverParts, method string, args interface{}, reply interface{}) error {
	// Handle a missing server
	if server == nil {
		return structs.ErrNoLeader
	}
	return r.connPool.RPC(r.config.Region, server.Addr, server.MajorVersion, method, args, reply)
}

// forwardServer is used to forward an RPC call to a particular server
func (r *rpcHandler) forwardServer(server *serverParts, method string, args interface{}, reply interface{}) error {
	// Handle a missing server
	if server == nil {
		return errors.New("must be given a valid server address")
	}
	return r.connPool.RPC(r.config.Region, server.Addr, server.MajorVersion, method, args, reply)
}

// forwardRegion is used to forward an RPC call to a remote region, or fail if no servers
func (r *rpcHandler) forwardRegion(region, method string, args interface{}, reply interface{}) error {
	// Bail if we can't find any servers
	r.peerLock.RLock()
	servers := r.peers[region]
	if len(servers) == 0 {
		r.peerLock.RUnlock()
		r.logger.Warn("no path found to region", "region", region)
		return structs.ErrNoRegionPath
	}

	// Select a random addr
	offset := rand.Intn(len(servers))
	server := servers[offset]
	r.peerLock.RUnlock()

	// Forward to remote Nomad
	metrics.IncrCounter([]string{"nomad", "rpc", "cross-region", region}, 1)
	return r.connPool.RPC(region, server.Addr, server.MajorVersion, method, args, reply)
}

// streamingRpc creates a connection to the given server and conducts the
// initial handshake, returning the connection or an error. It is the callers
// responsibility to close the connection if there is no returned error.
func (r *rpcHandler) streamingRpc(server *serverParts, method string) (net.Conn, error) {
	// Try to dial the server
	conn, err := net.DialTimeout("tcp", server.Addr.String(), 10*time.Second)
	if err != nil {
		return nil, err
	}

	// Cast to TCPConn
	if tcp, ok := conn.(*net.TCPConn); ok {
		tcp.SetKeepAlive(true)
		tcp.SetNoDelay(true)
	}

	if err := r.streamingRpcImpl(conn, server.Region, method); err != nil {
		return nil, err
	}

	return conn, nil
}

// streamingRpcImpl takes a pre-established connection to a server and conducts
// the handshake to establish a streaming RPC for the given method. If an error
// is returned, the underlying connection has been closed. Otherwise it is
// assumed that the connection has been hijacked by the RPC method.
func (r *rpcHandler) streamingRpcImpl(conn net.Conn, region, method string) error {
	// Check if TLS is enabled
	r.tlsWrapLock.RLock()
	tlsWrap := r.tlsWrap
	r.tlsWrapLock.RUnlock()

	if tlsWrap != nil {
		// Switch the connection into TLS mode
		if _, err := conn.Write([]byte{byte(pool.RpcTLS)}); err != nil {
			conn.Close()
			return err
		}

		// Wrap the connection in a TLS client
		tlsConn, err := tlsWrap(region, conn)
		if err != nil {
			conn.Close()
			return err
		}
		conn = tlsConn
	}

	// Write the multiplex byte to set the mode
	if _, err := conn.Write([]byte{byte(pool.RpcStreaming)}); err != nil {
		conn.Close()
		return err
	}

	// Send the header
	encoder := codec.NewEncoder(conn, structs.MsgpackHandle)
	decoder := codec.NewDecoder(conn, structs.MsgpackHandle)
	header := structs.StreamingRpcHeader{
		Method: method,
	}
	if err := encoder.Encode(header); err != nil {
		conn.Close()
		return err
	}

	// Wait for the acknowledgement
	var ack structs.StreamingRpcAck
	if err := decoder.Decode(&ack); err != nil {
		conn.Close()
		return err
	}

	if ack.Error != "" {
		conn.Close()
		return errors.New(ack.Error)
	}

	return nil
}

// raftApplyFuture is used to encode a message, run it through raft, and return the Raft future.
func (s *Server) raftApplyFuture(t structs.MessageType, msg interface{}) (raft.ApplyFuture, error) {
	buf, err := structs.Encode(t, msg)
	if err != nil {
		return nil, fmt.Errorf("Failed to encode request: %v", err)
	}

	// Warn if the command is very large
	if n := len(buf); n > raftWarnSize {
		s.logger.Warn("attempting to apply large raft entry", "raft_type", t, "bytes", n)
	}

	future := s.raft.Apply(buf, enqueueLimit)
	return future, nil
}

// raftApplyFn is the function signature for applying a msg to Raft
type raftApplyFn func(t structs.MessageType, msg interface{}) (interface{}, uint64, error)

// raftApply is used to encode a message, run it through raft, and return
// the FSM response along with any errors
func (s *Server) raftApply(t structs.MessageType, msg interface{}) (interface{}, uint64, error) {
	future, err := s.raftApplyFuture(t, msg)
	if err != nil {
		return nil, 0, err
	}
	if err := future.Error(); err != nil {
		return nil, 0, err
	}
	return future.Response(), future.Index(), nil
}

// setQueryMeta is used to populate the QueryMeta data for an RPC call
func (r *rpcHandler) setQueryMeta(m *structs.QueryMeta) {
	if r.IsLeader() {
		m.LastContact = 0
		m.KnownLeader = true
	} else {
		m.LastContact = time.Now().Sub(r.raft.LastContact())
		m.KnownLeader = (r.raft.Leader() != "")
	}
}

// queryFn is used to perform a query operation. If a re-query is needed, the
// passed-in watch set will be used to block for changes. The passed-in state
// store should be used (vs. calling fsm.State()) since the given state store
// will be correctly watched for changes if the state store is restored from
// a snapshot.
type queryFn func(memdb.WatchSet, *state.StateStore) error

// blockingOptions is used to parameterize blockingRPC
type blockingOptions struct {
	queryOpts *structs.QueryOptions
	queryMeta *structs.QueryMeta
	run       queryFn
}

// blockingRPC is used for queries that need to wait for a
// minimum index. This is used to block and wait for changes.
func (r *rpcHandler) blockingRPC(opts *blockingOptions) error {
	ctx := context.Background()
	var cancel context.CancelFunc
	var state *state.StateStore

	// Fast path non-blocking
	if opts.queryOpts.MinQueryIndex == 0 {
		goto RUN_QUERY
	}

	// Restrict the max query time, and ensure there is always one
	if opts.queryOpts.MaxQueryTime > maxQueryTime {
		opts.queryOpts.MaxQueryTime = maxQueryTime
	} else if opts.queryOpts.MaxQueryTime <= 0 {
		opts.queryOpts.MaxQueryTime = defaultQueryTime
	}

	// Apply a small amount of jitter to the request
	opts.queryOpts.MaxQueryTime += lib.RandomStagger(opts.queryOpts.MaxQueryTime / structs.JitterFraction)

	// Setup a query timeout
	ctx, cancel = context.WithTimeout(context.Background(), opts.queryOpts.MaxQueryTime)
	defer cancel()

RUN_QUERY:
	// Update the query meta data
	r.setQueryMeta(opts.queryMeta)

	// Increment the rpc query counter
	metrics.IncrCounter([]string{"nomad", "rpc", "query"}, 1)

	// We capture the state store and its abandon channel but pass a snapshot to
	// the blocking query function. We operate on the snapshot to allow separate
	// calls to the state store not all wrapped within the same transaction.
	state = r.fsm.State()
	abandonCh := state.AbandonCh()
	snap, _ := state.Snapshot()
	stateSnap := &snap.StateStore

	// We can skip all watch tracking if this isn't a blocking query.
	var ws memdb.WatchSet
	if opts.queryOpts.MinQueryIndex > 0 {
		ws = memdb.NewWatchSet()

		// This channel will be closed if a snapshot is restored and the
		// whole state store is abandoned.
		ws.Add(abandonCh)
	}

	// Block up to the timeout if we didn't see anything fresh.
	err := opts.run(ws, stateSnap)

	// Check for minimum query time
	if err == nil && opts.queryOpts.MinQueryIndex > 0 && opts.queryMeta.Index <= opts.queryOpts.MinQueryIndex {
		if err := ws.WatchCtx(ctx); err == nil {
			goto RUN_QUERY
		}
	}
	return err
}
