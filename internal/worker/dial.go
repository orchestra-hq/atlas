package worker

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/orchestra-hq/atlas/internal/core"
	"github.com/orchestra-hq/atlas/internal/tlsx"
	"github.com/orchestra-hq/atlas/internal/version"
	"github.com/orchestra-hq/atlas/internal/wire"
)

// Reconnect/heartbeat timings. Package-level vars (not consts) so tests can
// shrink them; production code never reassigns them.
var (
	heartbeatInterval = 10 * time.Second
	reconnectInitial  = 1 * time.Second
	reconnectMax      = 60 * time.Second
	// drainTimeout bounds how long a draining worker waits for its in-flight
	// requests to finish before forcing the connection closed, so a stuck request
	// cannot block shutdown indefinitely.
	drainTimeout = 30 * time.Second
)

// Inferencer is the worker-local inference target an execute message is
// dispatched to — the supervised engine adapter. *Worker satisfies it.
type Inferencer interface {
	Execute(ctx context.Context, req core.Request) (core.Response, error)
	ExecuteStream(ctx context.Context, req core.Request, sink core.StreamSink) error
	CountTokens(ctx context.Context, req core.Request) (int, error)
	Embed(ctx context.Context, req core.EmbedRequest) (core.EmbedResponse, error)
}

// ServedModel binds a model name clients address to the engine that answers it
// and the engine's context window. The worker advertises these in its join and
// routes execute/count_tokens messages by Request.Model.
type ServedModel struct {
	Name          string
	ContextWindow int
	// Class is the model class (M3 phase 2a, ADR-0012): empty means chat. The
	// worker advertises it so the gateway can route by class and reject a
	// wrong-class request (e.g. an embeddings call against a chat model).
	Class  string
	Engine Inferencer
}

// Loader launches a model instance on demand when the scheduler sends a load
// command (M1 phase 4b). It is implemented by the CLI, which owns engine
// provisioning, catalog resolution, and the model store; the worker holds one
// and calls it on load. engine names the engine the scheduler placed the model
// on — it must match the worker's configured engine. Load blocks until the
// engine is serving and returns the served model plus a stop func that tears the
// subprocess down (the worker calls it on unload or disconnect).
type Loader interface {
	Load(ctx context.Context, model, engine string) (ServedModel, func(), error)
}

// DialConfig configures a worker's outbound WebSocket connection (ADR-0007).
type DialConfig struct {
	// ServerURL is the ws:// or wss:// URL of the server's /workers/connect endpoint.
	ServerURL string
	// Token is the join token printed by 'atlas server'.
	Token string
	// Name is an optional human-readable label for this worker.
	Name string
	// Models are the model instances this worker serves; the gateway routes
	// inference to them over the channel. Empty is valid (a worker that only
	// reports its inventory, e.g. the phase-1 heartbeat path).
	Models []ServedModel
	// Drain, when closed, begins graceful shutdown of the active connection: the
	// worker announces a drain to the server, stops accepting new requests, lets
	// its in-flight requests finish, then disconnects. Dial returns nil once the
	// connection has drained. Nil disables it (the connection only ends on ctx
	// cancellation or a drop).
	Drain <-chan struct{}
	// Engine names the inference engine this worker runs (llamacpp/vllm); the
	// scheduler places only matching-engine models on it (M1 phase 4b).
	Engine string
	// TLSPin, when set, pins the server's leaf certificate for a wss:// join to a
	// self-signed deployment (ADR-0009): the worker accepts the connection only if
	// the presented cert matches this pin, in place of CA/hostname validation. It
	// must be a normalized "sha256:<hex>" pin (tlsx.NormalizePin). Empty leaves the
	// default system-trust verification, which is correct for ACME / public-CA
	// certs and for plain ws://.
	TLSPin string
	// Loader launches models on demand for scheduler-driven placement (M1 phase
	// 4b). Nil disables remote loading: the worker serves only its pre-declared
	// Models. Models a Loader launches are stopped when the connection ends, so a
	// reconnecting worker re-loads under the scheduler's reconcile.
	Loader Loader
	// Logger receives status events; defaults to slog.Default() if nil.
	Logger *slog.Logger
}

// Dial connects to the server hub, performs the join handshake, maintains the
// heartbeat loop, serves inference routed over the channel, and reconnects with
// exponential backoff on disconnect. It blocks until ctx is cancelled,
// returning ctx.Err().
func Dial(ctx context.Context, cfg DialConfig) error {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	hw := Detect()
	backoff := reconnectInitial

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A drain requested while reconnecting (no live connection to drain) is
		// just a stop: nothing in flight, so exit without reconnecting.
		if closed(cfg.Drain) {
			return nil
		}

		joined, drained, err := dialOnce(ctx, cfg, hw, log)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A completed drain ends the worker — do not reconnect. This covers both a
		// SIGTERM (cfg.Drain) and a server-initiated remove (a drain frame), so an
		// evicted worker stays gone rather than rejoining as a fresh worker.
		if drained {
			return nil
		}
		// A connection that successfully joined and then dropped is not a
		// failure to reach the server — reset the backoff so a healthy worker
		// that blips offline reconnects promptly rather than inheriting a long
		// delay accumulated over its uptime.
		if joined {
			backoff = reconnectInitial
		}
		if err != nil {
			log.Info("worker disconnected, reconnecting", "delay", backoff, "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-cfg.Drain:
				return nil
			case <-time.After(backoff):
			}
			if backoff < reconnectMax {
				backoff *= 2
				if backoff > reconnectMax {
					backoff = reconnectMax
				}
			}
		}
	}
}

// closed reports whether ch has been closed. A nil channel is never closed.
func closed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// dialOnce makes one connection attempt and runs it until it drops or ctx is
// cancelled. joined reports whether the join handshake completed (so the caller
// can reset reconnect backoff for a connection that was healthy before dropping,
// vs. one that never reached the server). drained reports whether the connection
// ended because the worker drained — either a SIGTERM or a server-initiated
// remove — so the caller stops rather than reconnecting.
// tlsDialer returns the WebSocket dialer for a connection. With no pin it is the
// default dialer (plain ws://, or wss:// validated against the system trust
// store — the ACME / public-CA case). With a pin it disables the default
// hostname/chain checks and instead accepts the connection only if the server's
// leaf cert matches the pin (ADR-0009 self-signed path).
func tlsDialer(pin string) *websocket.Dialer {
	if pin == "" {
		return websocket.DefaultDialer
	}
	d := *websocket.DefaultDialer
	d.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // not insecure: VerifyConnection pins the exact leaf cert
		VerifyConnection:   tlsx.PinnedVerifier(pin),
	}
	return &d
}

func dialOnce(ctx context.Context, cfg DialConfig, hw wire.Hardware, log *slog.Logger) (joined, drained bool, err error) {
	dialer := tlsDialer(cfg.TLSPin)
	conn, _, err := dialer.DialContext(ctx, cfg.ServerURL, nil)
	if err != nil {
		return false, false, fmt.Errorf("dial %s: %w", cfg.ServerURL, err)
	}
	defer func() { _ = conn.Close() }()
	// Bound every inbound frame so a misbehaving server cannot OOM the worker.
	conn.SetReadLimit(wire.MaxFrameBytes)

	models := make(map[string]ServedModel, len(cfg.Models))
	served := make([]wire.ServedModel, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		models[m.Name] = m
		served = append(served, wire.ServedModel{Name: m.Name, ContextWindow: m.ContextWindow, Class: m.Class})
	}

	joinMsg, err := wire.Encode(wire.MsgJoin, "", wire.JoinPayload{
		Token:    cfg.Token,
		Hardware: hw,
		Version:  version.String(),
		Name:     cfg.Name,
		Engine:   cfg.Engine,
		Models:   served,
	})
	if err != nil {
		return false, false, fmt.Errorf("encode join: %w", err)
	}
	if err := writeMsg(conn, joinMsg); err != nil {
		return false, false, fmt.Errorf("send join: %w", err)
	}

	_, ackData, err := conn.ReadMessage()
	if err != nil {
		return false, false, fmt.Errorf("read join_ack: %w", err)
	}
	var ackEnv wire.Message
	if err := json.Unmarshal(ackData, &ackEnv); err != nil || ackEnv.Type != wire.MsgJoinAck {
		return false, false, fmt.Errorf("expected join_ack, got %q", ackEnv.Type)
	}
	var ack wire.JoinAckPayload
	if err := json.Unmarshal(ackEnv.Payload, &ack); err != nil {
		return false, false, fmt.Errorf("parse join_ack: %w", err)
	}
	if !ack.Accepted {
		return false, false, fmt.Errorf("join rejected by server: %s", ack.Reason)
	}

	log.Info("joined server", "worker_id", ack.WorkerID, "server", cfg.ServerURL, "models", len(served))

	// connCtx bounds every goroutine and in-flight engine call to this
	// connection: when the loop returns (drop or shutdown) it is cancelled,
	// stopping the reader, request handlers, and their engine work.
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	sess := &session{
		conn:        conn,
		workerID:    ack.WorkerID,
		models:      models,
		loader:      cfg.Loader,
		loaded:      make(map[string]func()),
		log:         log,
		out:         make(chan wire.Message, 32),
		serverDrain: make(chan struct{}),
		drained:     make(chan struct{}),
		inflight:    make(map[string]context.CancelFunc),
	}
	// Models this connection loads on demand are torn down when it ends, so an
	// evicted or dropped worker leaves no orphan engines; the scheduler re-places
	// them on reconnect.
	defer sess.stopAllLoaded()

	// One reader goroutine; the main loop below is the sole writer, so every
	// frame (heartbeat, chunk, done, …) serialises through it without a
	// concurrent-write race (gorilla allows one writer at a time).
	incoming := make(chan []byte, 16)
	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				readErr <- err
				return
			}
			select {
			case incoming <- data:
			case <-connCtx.Done():
				return
			}
		}
	}()

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	// A dedicated writer goroutine is the connection's sole writer (gorilla
	// allows one at a time). Decoupling writes from the read/dispatch loop means
	// a slow or stalled socket write cannot delay processing an inbound frame —
	// the loop keeps draining `incoming`, so a cancel is honored promptly instead
	// of waiting out the write deadline.
	writeErr := make(chan error, 1)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-connCtx.Done():
				return
			case m := <-sess.out:
				if err := writeMsg(conn, m); err != nil {
					select {
					case writeErr <- fmt.Errorf("write %s: %w", m.Type, err):
					default:
					}
					return
				}
			}
		}
	}()

	// closeConn stops the writer and waits for it to exit before the main
	// goroutine writes the close frame, so there is never a concurrent writer.
	closeConn := func() {
		connCancel()
		<-writerDone
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutting down"))
	}

	// Drain triggers: drainTrigger is the external SIGTERM signal, serverDrainCh
	// fires when the server evicts us. Both are nil-ed once draining starts so
	// they cannot re-fire (a closed channel is always ready).
	drainTrigger := cfg.Drain
	serverDrainCh := sess.serverDrain
	var drainDeadline <-chan time.Time

	beginDrain := func(announce bool) {
		drainTrigger, serverDrainCh = nil, nil
		if announce {
			// Tell the server to stop routing to us. Best-effort and non-blocking:
			// the server also detects a leaving worker via the heartbeat timeout,
			// so a backed-up writer must not wedge the drain.
			if msg, err := wire.Encode(wire.MsgDrain, "", wire.DrainPayload{}); err == nil {
				select {
				case sess.out <- msg:
				default:
				}
			}
		}
		sess.startDraining()
		drainDeadline = time.After(drainTimeout)
		log.Info("worker draining", "timeout", drainTimeout)
	}

	for {
		select {
		case <-ctx.Done():
			// Hard shutdown (parent cancel, or a second signal): drop immediately.
			closeConn()
			return true, false, nil
		case <-drainTrigger:
			beginDrain(true)
		case <-serverDrainCh:
			beginDrain(false)
		case <-sess.drained:
			// In-flight requests finished: confirm the drain, then disconnect. The
			// writer is stopped first so the main goroutine can write drain_ack and
			// the close frame as the sole writer.
			connCancel()
			<-writerDone
			if ackMsg, err := wire.Encode(wire.MsgDrainAck, "", wire.DrainAckPayload{}); err == nil {
				_ = writeMsg(conn, ackMsg)
			}
			_ = conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "drained"))
			return true, true, nil
		case <-drainDeadline:
			log.Warn("drain timeout exceeded; forcing disconnect", "timeout", drainTimeout)
			closeConn()
			return true, true, nil
		case err := <-readErr:
			return true, false, err
		case err := <-writeErr:
			return true, false, err
		case data := <-incoming:
			sess.dispatch(connCtx, data)
		case <-heartbeat.C:
			hbMsg, err := wire.Encode(wire.MsgHeartbeat, "", wire.HeartbeatPayload{WorkerID: ack.WorkerID})
			if err != nil {
				return true, false, err
			}
			// Best-effort: if the writer is backed up (a stalled socket), skip
			// this heartbeat rather than block the loop. The stuck write trips its
			// deadline and surfaces via writeErr, tearing the connection down
			// within the server's heartbeat-timeout window anyway.
			select {
			case sess.out <- hbMsg:
			default:
			}
		}
	}
}

// session holds the per-connection state shared between the main loop and the
// request-handler goroutines it spawns.
type session struct {
	conn     *websocket.Conn
	workerID string
	log      *slog.Logger

	loader Loader

	// out carries frames produced by handler goroutines to the main loop, the
	// sole writer of conn.
	out chan wire.Message

	// serverDrain is closed once (serverDrainOnce) when the server sends a drain
	// frame, so the main loop begins draining; drained is closed once draining is
	// active and the last in-flight request finishes.
	serverDrain     chan struct{}
	serverDrainOnce sync.Once
	drained         chan struct{}
	drainedOnce     sync.Once

	mu       sync.Mutex
	draining bool // set under mu; once true, new requests are refused
	closed   bool // set under mu by stopAllLoaded; the connection has torn down
	inflight map[string]context.CancelFunc
	models   map[string]ServedModel // served models, mutated by load/unload
	loaded   map[string]func()      // stop funcs for models this connection loaded
}

// startDraining marks the session draining so new requests are refused. If no
// requests are in flight it signals drained immediately.
func (s *session) startDraining() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draining = true
	if len(s.inflight) == 0 {
		s.drainedOnce.Do(func() { close(s.drained) })
	}
}

// signalServerDrain wakes the main loop to begin draining on a server-sent drain.
func (s *session) signalServerDrain() {
	s.serverDrainOnce.Do(func() { close(s.serverDrain) })
}

// dispatch demuxes one inbound frame: control acks are ignored, execute and
// count_tokens spawn handler goroutines, cancel stops an in-flight request.
func (s *session) dispatch(ctx context.Context, data []byte) {
	var m wire.Message
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	switch m.Type {
	case wire.MsgHeartbeatAck:
		// Liveness only; nothing to do.
	case wire.MsgDrain:
		// The server is evicting this worker (atlas workers remove). Begin the
		// same drain the main loop runs for a SIGTERM.
		s.signalServerDrain()
	case wire.MsgExecute:
		var p wire.ExecutePayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			return
		}
		// Refuse new work once draining: the server has already stopped routing,
		// but a request may have crossed in transit. A retryable error lets the
		// gateway send it elsewhere rather than have it abort when we disconnect.
		if s.refuseIfDraining(ctx, p.RequestID) {
			return
		}
		go s.handleExecute(ctx, p)
	case wire.MsgCountTokens:
		var p wire.CountTokensPayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			return
		}
		if s.refuseIfDraining(ctx, p.RequestID) {
			return
		}
		go s.handleCountTokens(ctx, p)
	case wire.MsgEmbed:
		var p wire.EmbedPayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			return
		}
		if s.refuseIfDraining(ctx, p.RequestID) {
			return
		}
		go s.handleEmbed(ctx, p)
	case wire.MsgCancel:
		var p wire.CancelPayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			return
		}
		s.cancelInflight(p.RequestID)
	case wire.MsgLoad:
		var p wire.LoadPayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			return
		}
		// Loading boots an engine subprocess (slow), so run it off the dispatch
		// loop, which must stay free to process inference and heartbeats.
		go s.handleLoad(ctx, p)
	case wire.MsgUnload:
		var p wire.UnloadPayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			return
		}
		go s.handleUnload(ctx, p)
	}
}

// handleLoad launches the requested model via the worker's Loader and registers
// it in the served set, then reports model_ready; on failure it reports
// load_failed. A load while draining, with no Loader, or for an already-served
// model is handled without starting a duplicate engine.
func (s *session) handleLoad(ctx context.Context, p wire.LoadPayload) {
	s.mu.Lock()
	draining := s.draining
	existing, already := s.models[p.Model]
	s.mu.Unlock()

	switch {
	case s.loader == nil:
		_ = s.enqueue(ctx, wire.MsgLoadFailed, wire.LoadFailedPayload{Model: p.Model, Reason: "worker has no loader"})
		return
	case draining:
		_ = s.enqueue(ctx, wire.MsgLoadFailed, wire.LoadFailedPayload{Model: p.Model, Reason: "worker draining"})
		return
	case already:
		// Idempotent: already serving this model — re-confirm it is ready.
		_ = s.enqueue(ctx, wire.MsgModelReady, wire.ModelReadyPayload{Model: p.Model, ContextWindow: existing.ContextWindow, Class: existing.Class})
		return
	}

	served, stop, err := s.loader.Load(ctx, p.Model, p.Engine)
	if err != nil {
		_ = s.enqueue(ctx, wire.MsgLoadFailed, wire.LoadFailedPayload{Model: p.Model, Reason: err.Error()})
		return
	}

	// Re-check under lock: the connection may have torn down or begun draining, or
	// a concurrent load may have won, while the engine booted.
	s.mu.Lock()
	if s.closed {
		// stopAllLoaded already swept the loaded set, so registering this engine now
		// would orphan it past the connection's life. Stop it instead.
		s.mu.Unlock()
		stop()
		return
	}
	if s.draining {
		s.mu.Unlock()
		stop()
		_ = s.enqueue(ctx, wire.MsgLoadFailed, wire.LoadFailedPayload{Model: p.Model, Reason: "worker draining"})
		return
	}
	if _, dup := s.models[served.Name]; dup {
		s.mu.Unlock()
		stop() // keep the instance already serving this name
	} else {
		s.models[served.Name] = served
		s.loaded[served.Name] = stop
		s.mu.Unlock()
	}
	s.log.Info("loaded model", "model", served.Name, "context_window", served.ContextWindow, "class", served.Class)
	_ = s.enqueue(ctx, wire.MsgModelReady, wire.ModelReadyPayload{Model: served.Name, ContextWindow: served.ContextWindow, Class: served.Class})
}

// handleUnload stops a model this connection loaded and reports model_unloaded.
// Models the worker did not load itself (pre-declared --model) are left running;
// the scheduler only unloads what it placed.
func (s *session) handleUnload(ctx context.Context, p wire.UnloadPayload) {
	s.mu.Lock()
	stop, ok := s.loaded[p.Model]
	if ok {
		delete(s.loaded, p.Model)
		delete(s.models, p.Model)
	}
	s.mu.Unlock()
	if !ok {
		return // not ours to stop
	}
	if stop != nil {
		stop()
	}
	s.log.Info("unloaded model", "model", p.Model)
	_ = s.enqueue(ctx, wire.MsgModelUnloaded, wire.ModelUnloadedPayload(p))
}

// stopAllLoaded tears down every model this connection loaded on demand, called
// when the connection ends so no orphan engines outlive it.
func (s *session) stopAllLoaded() {
	s.mu.Lock()
	// Mark the session torn down before sweeping so a handleLoad whose engine is
	// still booting sees closed on its post-Load re-check and stops the engine
	// rather than registering it into a set this sweep already passed.
	s.closed = true
	stops := make([]func(), 0, len(s.loaded))
	for name, stop := range s.loaded {
		stops = append(stops, stop)
		delete(s.loaded, name)
		delete(s.models, name)
	}
	s.mu.Unlock()
	for _, stop := range stops {
		if stop != nil {
			stop()
		}
	}
}

// lookupModel returns the served model for name under lock (the served set is
// mutated by load/unload).
func (s *session) lookupModel(name string) (ServedModel, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.models[name]
	return m, ok
}

// refuseIfDraining rejects a new request with a retryable error when the session
// is draining, returning true if it did so (the caller must not spawn a handler).
func (s *session) refuseIfDraining(ctx context.Context, requestID string) bool {
	s.mu.Lock()
	draining := s.draining
	s.mu.Unlock()
	if !draining {
		return false
	}
	_ = s.enqueue(ctx, wire.MsgError, wire.ErrorPayload{
		RequestID: requestID, Code: wire.CodeEngineUnavailable, Message: "worker draining",
	})
	return true
}

// handleExecute runs one inference request against the engine for the requested
// model and streams the result back. Server-initiated cancellation (a cancel
// message or a dropped connection) ends the request silently; any other engine
// failure is reported as an error frame.
func (s *session) handleExecute(ctx context.Context, p wire.ExecutePayload) {
	model, ok := s.lookupModel(p.Request.Model)
	if !ok {
		_ = s.enqueue(ctx, wire.MsgError, wire.ErrorPayload{
			RequestID: p.RequestID, Code: wire.CodeInternal,
			Message: "worker does not serve model " + p.Request.Model,
		})
		return
	}

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.register(p.RequestID, cancel)
	defer s.unregister(p.RequestID)

	var err error
	if p.Stream {
		sink := core.EventSink{
			Emit: func(e core.StreamEvent) error {
				return s.enqueue(reqCtx, wire.MsgChunk, wire.ChunkPayload{RequestID: p.RequestID, Event: e})
			},
			OnDone: func(r core.StopReason, u core.Usage) error {
				return s.enqueue(reqCtx, wire.MsgDone, wire.DonePayload{RequestID: p.RequestID, StopReason: r, Usage: u})
			},
		}
		err = model.Engine.ExecuteStream(reqCtx, p.Request, sink)
	} else {
		var resp core.Response
		resp, err = model.Engine.Execute(reqCtx, p.Request)
		if err == nil {
			err = s.enqueue(reqCtx, wire.MsgResponse, wire.ResponsePayload{RequestID: p.RequestID, Response: resp})
		}
	}
	if err != nil && reqCtx.Err() == nil {
		_ = s.enqueue(ctx, wire.MsgError, wire.ErrorPayload{RequestID: p.RequestID, Code: errorCode(err), Message: err.Error()})
	}
}

// handleCountTokens answers a count_tokens request with the engine tokenizer's
// count, or an error frame on failure.
func (s *session) handleCountTokens(ctx context.Context, p wire.CountTokensPayload) {
	model, ok := s.lookupModel(p.Request.Model)
	if !ok {
		_ = s.enqueue(ctx, wire.MsgError, wire.ErrorPayload{
			RequestID: p.RequestID, Code: wire.CodeInternal,
			Message: "worker does not serve model " + p.Request.Model,
		})
		return
	}

	// Register for cancellation like handleExecute, so a server-sent cancel (or a
	// client disconnect) aborts the engine's tokenizer call instead of letting it
	// run to completion against an already-abandoned request.
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.register(p.RequestID, cancel)
	defer s.unregister(p.RequestID)

	n, err := model.Engine.CountTokens(reqCtx, p.Request)
	if err != nil {
		if reqCtx.Err() == nil {
			_ = s.enqueue(ctx, wire.MsgError, wire.ErrorPayload{RequestID: p.RequestID, Code: errorCode(err), Message: err.Error()})
		}
		return
	}
	_ = s.enqueue(reqCtx, wire.MsgTokenCount, wire.TokenCountPayload{RequestID: p.RequestID, Count: n})
}

// handleEmbed answers an embed request with the engine's vectors (M3 phase 2a). It
// mirrors handleCountTokens: single-shot, cancellable, replying with an embed_result
// on success or an error frame on failure.
func (s *session) handleEmbed(ctx context.Context, p wire.EmbedPayload) {
	model, ok := s.lookupModel(p.Request.Model)
	if !ok {
		_ = s.enqueue(ctx, wire.MsgError, wire.ErrorPayload{
			RequestID: p.RequestID, Code: wire.CodeInternal,
			Message: "worker does not serve model " + p.Request.Model,
		})
		return
	}

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.register(p.RequestID, cancel)
	defer s.unregister(p.RequestID)

	resp, err := model.Engine.Embed(reqCtx, p.Request)
	if err != nil {
		if reqCtx.Err() == nil {
			_ = s.enqueue(ctx, wire.MsgError, wire.ErrorPayload{RequestID: p.RequestID, Code: errorCode(err), Message: err.Error()})
		}
		return
	}
	_ = s.enqueue(reqCtx, wire.MsgEmbedResult, wire.EmbedResultPayload{RequestID: p.RequestID, Response: resp})
}

// enqueue hands a frame to the main loop for writing, returning ctx.Err() if the
// connection drops first so a streaming engine call aborts rather than blocking.
func (s *session) enqueue(ctx context.Context, typ wire.MessageType, payload any) error {
	msg, err := wire.Encode(typ, "", payload)
	if err != nil {
		return err
	}
	select {
	case s.out <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *session) register(id string, cancel context.CancelFunc) {
	s.mu.Lock()
	s.inflight[id] = cancel
	s.mu.Unlock()
}

func (s *session) unregister(id string) {
	s.mu.Lock()
	delete(s.inflight, id)
	// The last in-flight request finishing during a drain completes it.
	if s.draining && len(s.inflight) == 0 {
		s.drainedOnce.Do(func() { close(s.drained) })
	}
	s.mu.Unlock()
}

func (s *session) cancelInflight(id string) {
	s.mu.Lock()
	if cancel, ok := s.inflight[id]; ok {
		cancel()
	}
	s.mu.Unlock()
}

// errorCode classifies an engine failure for the wire so the gateway can
// reconstruct the client-facing status: an unavailable engine is retryable
// (529), anything else is an internal error.
func errorCode(err error) string {
	if errors.Is(err, core.ErrEngineUnavailable) {
		return wire.CodeEngineUnavailable
	}
	return wire.CodeInternal
}

func writeMsg(conn *websocket.Conn, msg wire.Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	return conn.WriteMessage(websocket.TextMessage, data)
}
