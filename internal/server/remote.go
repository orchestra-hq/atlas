package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/orchestra-hq/atlas/internal/core"
	"github.com/orchestra-hq/atlas/internal/wire"
)

// remoteWorker is the gateway's view of one connected worker reached over the
// WebSocket channel (ADR-0007). It implements Executor, StreamExecutor, and
// TokenCounter, so the gateway dispatches to a remote worker exactly as it does
// the in-process one — it never learns which it is talking to.
//
// One connection multiplexes every in-flight request. The hub's read loop is
// the sole reader and feeds inference responses to route; a single write pump
// is the sole writer (gorilla allows one of each concurrently). Each request
// gets an id and a pending entry; responses are demuxed back to the waiting
// goroutine by that id.
type remoteWorker struct {
	conn *websocket.Conn

	out  chan wire.Message // frames to the write pump
	done chan struct{}     // closed once the connection is torn down

	closeOnce sync.Once
	idSeq     atomic.Uint64

	mu      sync.Mutex
	pending map[string]*pendingReq
}

// pendingReq is one in-flight request awaiting responses. ch delivers inference
// frames in arrival order; abandoned is closed when the waiting goroutine
// returns, so the read loop drops late frames instead of blocking on it;
// overflow is closed by route when ch fills (a consumer too slow to drain),
// ending just that request rather than stalling the shared reader.
type pendingReq struct {
	ch        chan reqEvent
	abandoned chan struct{}
	overflow  chan struct{}
}

// reqEvent is one decoded worker→server inference frame routed to a pending
// request. Kind selects which fields are set.
type reqEvent struct {
	kind   wire.MessageType
	event  core.StreamEvent  // MsgChunk
	resp   core.Response     // MsgResponse
	reason core.StopReason   // MsgDone
	usage  core.Usage        // MsgDone
	count  int               // MsgTokenCount
	errp   wire.ErrorPayload // MsgError
}

// pendingBufferSize is how many inference frames a single request may buffer
// before a non-draining consumer is treated as too slow and the request is
// overflowed. Generous enough to absorb normal scheduling jitter between the
// reader and a request's consumer goroutine.
const pendingBufferSize = 64

// errSlowConsumer ends a single request whose response buffer overflowed because
// its consumer (typically a slow SSE client) stopped draining. Failing just this
// request is how the shared connection reader stays unblocked for everyone else.
var errSlowConsumer = errors.New("server: response buffer overflow (consumer too slow)")

func newRemoteWorker(conn *websocket.Conn) *remoteWorker {
	return &remoteWorker{
		conn:    conn,
		out:     make(chan wire.Message, 32),
		done:    make(chan struct{}),
		pending: make(map[string]*pendingReq),
	}
}

// writePump is the connection's sole writer: it drains out until the connection
// is torn down or a write fails (a failed write means the conn is dead, so it
// closes to fail in-flight requests).
func (rw *remoteWorker) writePump() {
	for {
		select {
		case <-rw.done:
			return
		case msg := <-rw.out:
			if err := writeFrame(rw.conn, msg); err != nil {
				rw.close()
				return
			}
		}
	}
}

// close tears the connection down: it unblocks every waiting request (a drop
// mid-request surfaces as a retryable overload) and stops the write pump. The
// hub still owns closing the underlying conn.
func (rw *remoteWorker) close() {
	rw.closeOnce.Do(func() { close(rw.done) })
}

// route delivers one worker→server inference frame to its pending request, or
// drops it if the request is unknown (e.g. a late chunk after a stop sequence
// cut the stream) or already abandoned.
//
// It runs inline on the hub's sole connection reader, so it must never block:
// if the request's buffer is full because its consumer stopped draining (a slow
// SSE client), it fails that one request via overflow instead of stalling the
// reader and head-of-line-blocking every other multiplexed request and the
// heartbeat handling on this connection.
func (rw *remoteWorker) route(m wire.Message) {
	ev, id, ok := decodeReqEvent(m)
	if !ok {
		return
	}
	rw.mu.Lock()
	p, ok := rw.pending[id]
	rw.mu.Unlock()
	if !ok {
		return
	}
	select {
	case p.ch <- ev:
	case <-p.abandoned:
	case <-rw.done:
	default:
		rw.overflow(id)
	}
}

// overflow ends a request whose response buffer filled: its consumer is too slow
// to keep up. Closing overflow wakes the waiting goroutine with errSlowConsumer,
// and deleting the entry makes route drop any further frames for it.
func (rw *remoteWorker) overflow(id string) {
	rw.mu.Lock()
	if p, ok := rw.pending[id]; ok {
		close(p.overflow)
		delete(rw.pending, id)
	}
	rw.mu.Unlock()
}

// sendAck enqueues a control-plane ack (the hub's heartbeat reply) through the
// single write pump.
func (rw *remoteWorker) sendAck(typ wire.MessageType, payload any) {
	msg, err := wire.Encode(typ, "", payload)
	if err != nil {
		return
	}
	select {
	case rw.out <- msg:
	case <-rw.done:
	}
}

// Execute runs one buffered inference request on the worker and returns its
// response. It satisfies Executor.
func (rw *remoteWorker) Execute(ctx context.Context, req core.Request) (core.Response, error) {
	id, p := rw.begin()
	defer rw.end(id)

	if err := rw.send(ctx, wire.MsgExecute, wire.ExecutePayload{RequestID: id, Stream: false, Request: req}); err != nil {
		return core.Response{}, err
	}
	for {
		select {
		case ev := <-p.ch:
			switch ev.kind {
			case wire.MsgResponse:
				return ev.resp, nil
			case wire.MsgError:
				return core.Response{}, wireError(ev.errp)
			}
		case <-p.overflow:
			rw.cancel(id)
			return core.Response{}, errSlowConsumer
		case <-ctx.Done():
			rw.cancel(id)
			return core.Response{}, ctx.Err()
		case <-rw.done:
			return core.Response{}, core.ErrEngineUnavailable
		}
	}
}

// ExecuteStream runs one streaming inference request, replaying each delta onto
// sink. It satisfies StreamExecutor. A sink that returns ErrStopStreaming (the
// gateway's stop-sequence match) ends the stream and cancels the remote
// generation; ctx cancellation does the same.
func (rw *remoteWorker) ExecuteStream(ctx context.Context, req core.Request, sink core.StreamSink) error {
	id, p := rw.begin()
	defer rw.end(id)

	if err := rw.send(ctx, wire.MsgExecute, wire.ExecutePayload{RequestID: id, Stream: true, Request: req}); err != nil {
		return err
	}
	for {
		select {
		case ev := <-p.ch:
			switch ev.kind {
			case wire.MsgChunk:
				if err := ev.event.ApplyTo(sink); err != nil {
					rw.cancel(id)
					if errors.Is(err, core.ErrStopStreaming) {
						return nil
					}
					return err
				}
			case wire.MsgDone:
				return sink.Done(ev.reason, ev.usage)
			case wire.MsgError:
				return wireError(ev.errp)
			}
		case <-p.overflow:
			rw.cancel(id)
			return errSlowConsumer
		case <-ctx.Done():
			rw.cancel(id)
			return ctx.Err()
		case <-rw.done:
			return core.ErrEngineUnavailable
		}
	}
}

// CountTokens returns the prompt's token count from the worker engine's
// tokenizer. It satisfies TokenCounter.
func (rw *remoteWorker) CountTokens(ctx context.Context, req core.Request) (int, error) {
	id, p := rw.begin()
	defer rw.end(id)

	if err := rw.send(ctx, wire.MsgCountTokens, wire.CountTokensPayload{RequestID: id, Request: req}); err != nil {
		return 0, err
	}
	for {
		select {
		case ev := <-p.ch:
			switch ev.kind {
			case wire.MsgTokenCount:
				return ev.count, nil
			case wire.MsgError:
				return 0, wireError(ev.errp)
			}
		case <-p.overflow:
			rw.cancel(id)
			return 0, errSlowConsumer
		case <-ctx.Done():
			rw.cancel(id)
			return 0, ctx.Err()
		case <-rw.done:
			return 0, core.ErrEngineUnavailable
		}
	}
}

// begin allocates a request id and registers its pending entry.
func (rw *remoteWorker) begin() (string, *pendingReq) {
	id := "r" + strconv.FormatUint(rw.idSeq.Add(1), 10)
	p := &pendingReq{ch: make(chan reqEvent, pendingBufferSize), abandoned: make(chan struct{}), overflow: make(chan struct{})}
	rw.mu.Lock()
	rw.pending[id] = p
	rw.mu.Unlock()
	return id, p
}

// end removes a request's pending entry and signals route to drop any frames
// still arriving for it.
func (rw *remoteWorker) end(id string) {
	rw.mu.Lock()
	if p, ok := rw.pending[id]; ok {
		close(p.abandoned)
		delete(rw.pending, id)
	}
	rw.mu.Unlock()
}

// send enqueues a frame through the write pump, returning early if ctx is
// cancelled or the worker drops (a drop surfaces as a retryable overload).
func (rw *remoteWorker) send(ctx context.Context, typ wire.MessageType, payload any) error {
	msg, err := wire.Encode(typ, "", payload)
	if err != nil {
		return err
	}
	select {
	case rw.out <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-rw.done:
		return core.ErrEngineUnavailable
	}
}

// cancel best-effort asks the worker to stop an in-flight request. It never
// blocks: cancellation is an optimisation (the gateway has already stopped
// reading), so a full write queue or a dead conn just drops it.
func (rw *remoteWorker) cancel(id string) {
	msg, err := wire.Encode(wire.MsgCancel, "", wire.CancelPayload{RequestID: id})
	if err != nil {
		return
	}
	select {
	case rw.out <- msg:
	default:
	}
}

// decodeReqEvent parses a worker→server inference frame into a reqEvent and its
// request id. ok is false for frames that are not request responses.
func decodeReqEvent(m wire.Message) (ev reqEvent, id string, ok bool) {
	switch m.Type {
	case wire.MsgResponse:
		var p wire.ResponsePayload
		if json.Unmarshal(m.Payload, &p) != nil {
			return reqEvent{}, "", false
		}
		return reqEvent{kind: m.Type, resp: p.Response}, p.RequestID, true
	case wire.MsgChunk:
		var p wire.ChunkPayload
		if json.Unmarshal(m.Payload, &p) != nil {
			return reqEvent{}, "", false
		}
		return reqEvent{kind: m.Type, event: p.Event}, p.RequestID, true
	case wire.MsgDone:
		var p wire.DonePayload
		if json.Unmarshal(m.Payload, &p) != nil {
			return reqEvent{}, "", false
		}
		return reqEvent{kind: m.Type, reason: p.StopReason, usage: p.Usage}, p.RequestID, true
	case wire.MsgTokenCount:
		var p wire.TokenCountPayload
		if json.Unmarshal(m.Payload, &p) != nil {
			return reqEvent{}, "", false
		}
		return reqEvent{kind: m.Type, count: p.Count}, p.RequestID, true
	case wire.MsgError:
		var p wire.ErrorPayload
		if json.Unmarshal(m.Payload, &p) != nil {
			return reqEvent{}, "", false
		}
		return reqEvent{kind: m.Type, errp: p}, p.RequestID, true
	default:
		return reqEvent{}, "", false
	}
}

// wireError reconstructs a gateway-facing error from a worker error frame: an
// unavailable engine maps to the retryable overload path (529), anything else
// is an opaque internal failure.
func wireError(e wire.ErrorPayload) error {
	if e.Code == wire.CodeEngineUnavailable {
		return core.ErrEngineUnavailable
	}
	return fmt.Errorf("worker error: %s", e.Message)
}

// writeFrame marshals and writes one frame, under a write deadline so a stuck
// socket cannot wedge the write pump indefinitely.
func writeFrame(conn *websocket.Conn, msg wire.Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	return conn.WriteMessage(websocket.TextMessage, data)
}
