package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/orchestra-hq/atlas/internal/core"
	"github.com/orchestra-hq/atlas/internal/version"
	"github.com/orchestra-hq/atlas/internal/wire"
)

// Reconnect/heartbeat timings. Package-level vars (not consts) so tests can
// shrink them; production code never reassigns them.
var (
	heartbeatInterval = 10 * time.Second
	reconnectInitial  = 1 * time.Second
	reconnectMax      = 60 * time.Second
)

// Inferencer is the worker-local inference target an execute message is
// dispatched to — the supervised engine adapter. *Worker satisfies it.
type Inferencer interface {
	Execute(ctx context.Context, req core.Request) (core.Response, error)
	ExecuteStream(ctx context.Context, req core.Request, sink core.StreamSink) error
	CountTokens(ctx context.Context, req core.Request) (int, error)
}

// ServedModel binds a model name clients address to the engine that answers it
// and the engine's context window. The worker advertises these in its join and
// routes execute/count_tokens messages by Request.Model.
type ServedModel struct {
	Name          string
	ContextWindow int
	Engine        Inferencer
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

		joined, err := dialOnce(ctx, cfg, hw, log)
		if ctx.Err() != nil {
			return ctx.Err()
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

// dialOnce makes one connection attempt and runs it until it drops or ctx is
// cancelled. The returned joined reports whether the join handshake completed
// (so the caller can reset reconnect backoff for a connection that was healthy
// before dropping, vs. one that never reached the server).
func dialOnce(ctx context.Context, cfg DialConfig, hw wire.Hardware, log *slog.Logger) (joined bool, err error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, cfg.ServerURL, nil)
	if err != nil {
		return false, fmt.Errorf("dial %s: %w", cfg.ServerURL, err)
	}
	defer func() { _ = conn.Close() }()

	models := make(map[string]ServedModel, len(cfg.Models))
	served := make([]wire.ServedModel, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		models[m.Name] = m
		served = append(served, wire.ServedModel{Name: m.Name, ContextWindow: m.ContextWindow})
	}

	joinMsg, err := wire.Encode(wire.MsgJoin, "", wire.JoinPayload{
		Token:    cfg.Token,
		Hardware: hw,
		Version:  version.String(),
		Name:     cfg.Name,
		Models:   served,
	})
	if err != nil {
		return false, fmt.Errorf("encode join: %w", err)
	}
	if err := writeMsg(conn, joinMsg); err != nil {
		return false, fmt.Errorf("send join: %w", err)
	}

	_, ackData, err := conn.ReadMessage()
	if err != nil {
		return false, fmt.Errorf("read join_ack: %w", err)
	}
	var ackEnv wire.Message
	if err := json.Unmarshal(ackData, &ackEnv); err != nil || ackEnv.Type != wire.MsgJoinAck {
		return false, fmt.Errorf("expected join_ack, got %q", ackEnv.Type)
	}
	var ack wire.JoinAckPayload
	if err := json.Unmarshal(ackEnv.Payload, &ack); err != nil {
		return false, fmt.Errorf("parse join_ack: %w", err)
	}
	if !ack.Accepted {
		return false, fmt.Errorf("join rejected by server: %s", ack.Reason)
	}

	log.Info("joined server", "worker_id", ack.WorkerID, "server", cfg.ServerURL, "models", len(served))

	// connCtx bounds every goroutine and in-flight engine call to this
	// connection: when the loop returns (drop or shutdown) it is cancelled,
	// stopping the reader, request handlers, and their engine work.
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	sess := &session{
		conn:     conn,
		workerID: ack.WorkerID,
		models:   models,
		log:      log,
		out:      make(chan wire.Message, 32),
		inflight: make(map[string]context.CancelFunc),
	}

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

	for {
		select {
		case <-ctx.Done():
			_ = conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutting down"))
			return true, nil
		case err := <-readErr:
			return true, err
		case data := <-incoming:
			sess.dispatch(connCtx, data)
		case m := <-sess.out:
			if err := writeMsg(conn, m); err != nil {
				return true, fmt.Errorf("write %s: %w", m.Type, err)
			}
		case <-heartbeat.C:
			hbMsg, err := wire.Encode(wire.MsgHeartbeat, "", wire.HeartbeatPayload{WorkerID: ack.WorkerID})
			if err != nil {
				return true, err
			}
			if err := writeMsg(conn, hbMsg); err != nil {
				return true, fmt.Errorf("send heartbeat: %w", err)
			}
		}
	}
}

// session holds the per-connection state shared between the main loop and the
// request-handler goroutines it spawns.
type session struct {
	conn     *websocket.Conn
	workerID string
	models   map[string]ServedModel
	log      *slog.Logger

	// out carries frames produced by handler goroutines to the main loop, the
	// sole writer of conn.
	out chan wire.Message

	mu       sync.Mutex
	inflight map[string]context.CancelFunc
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
	case wire.MsgExecute:
		var p wire.ExecutePayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			return
		}
		go s.handleExecute(ctx, p)
	case wire.MsgCountTokens:
		var p wire.CountTokensPayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			return
		}
		go s.handleCountTokens(ctx, p)
	case wire.MsgCancel:
		var p wire.CancelPayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			return
		}
		s.cancelInflight(p.RequestID)
	}
}

// handleExecute runs one inference request against the engine for the requested
// model and streams the result back. Server-initiated cancellation (a cancel
// message or a dropped connection) ends the request silently; any other engine
// failure is reported as an error frame.
func (s *session) handleExecute(ctx context.Context, p wire.ExecutePayload) {
	model, ok := s.models[p.Request.Model]
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
	model, ok := s.models[p.Request.Model]
	if !ok {
		_ = s.enqueue(ctx, wire.MsgError, wire.ErrorPayload{
			RequestID: p.RequestID, Code: wire.CodeInternal,
			Message: "worker does not serve model " + p.Request.Model,
		})
		return
	}
	n, err := model.Engine.CountTokens(ctx, p.Request)
	if err != nil {
		if ctx.Err() == nil {
			_ = s.enqueue(ctx, wire.MsgError, wire.ErrorPayload{RequestID: p.RequestID, Code: errorCode(err), Message: err.Error()})
		}
		return
	}
	_ = s.enqueue(ctx, wire.MsgTokenCount, wire.TokenCountPayload{RequestID: p.RequestID, Count: n})
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
