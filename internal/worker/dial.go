package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/gorilla/websocket"

	"github.com/orchestra-hq/atlas/internal/version"
	"github.com/orchestra-hq/atlas/internal/wire"
)

const (
	heartbeatInterval = 10 * time.Second
	reconnectInitial  = 1 * time.Second
	reconnectMax      = 60 * time.Second
)

// DialConfig configures a worker's outbound WebSocket connection (ADR-0007).
type DialConfig struct {
	// ServerURL is the ws:// or wss:// URL of the server's /workers/connect endpoint.
	ServerURL string
	// Token is the join token printed by 'atlas server'.
	Token string
	// Name is an optional human-readable label for this worker.
	Name string
	// Logger receives status events; defaults to slog.Default() if nil.
	Logger *slog.Logger
}

// Dial connects to the server hub, performs the join handshake, maintains the
// heartbeat loop, and reconnects with exponential backoff on disconnect. It
// blocks until ctx is cancelled, returning ctx.Err().
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

		err := dialOnce(ctx, cfg, hw, log)
		if ctx.Err() != nil {
			return ctx.Err()
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

func dialOnce(ctx context.Context, cfg DialConfig, hw wire.Hardware, log *slog.Logger) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, cfg.ServerURL, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", cfg.ServerURL, err)
	}
	defer func() { _ = conn.Close() }()

	joinMsg, err := wire.Encode(wire.MsgJoin, "", wire.JoinPayload{
		Token:    cfg.Token,
		Hardware: hw,
		Version:  version.String(),
		Name:     cfg.Name,
	})
	if err != nil {
		return fmt.Errorf("encode join: %w", err)
	}
	if err := writeMsg(conn, joinMsg); err != nil {
		return fmt.Errorf("send join: %w", err)
	}

	_, ackData, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read join_ack: %w", err)
	}
	var ackEnv wire.Message
	if err := json.Unmarshal(ackData, &ackEnv); err != nil || ackEnv.Type != wire.MsgJoinAck {
		return fmt.Errorf("expected join_ack, got %q", ackEnv.Type)
	}
	var ack wire.JoinAckPayload
	if err := json.Unmarshal(ackEnv.Payload, &ack); err != nil {
		return fmt.Errorf("parse join_ack: %w", err)
	}
	if !ack.Accepted {
		return fmt.Errorf("join rejected by server: %s", ack.Reason)
	}

	log.Info("joined server", "worker_id", ack.WorkerID, "server", cfg.ServerURL)

	// Background reader: accepts heartbeat_ack and future server messages
	// (execute in phase 2). Sends any read error to readErr.
	readErr := make(chan error, 1)
	go func() {
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				readErr <- err
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
			return nil
		case err := <-readErr:
			return err
		case <-heartbeat.C:
			hbMsg, err := wire.Encode(wire.MsgHeartbeat, "", wire.HeartbeatPayload{WorkerID: ack.WorkerID})
			if err != nil {
				return err
			}
			if err := writeMsg(conn, hbMsg); err != nil {
				return fmt.Errorf("send heartbeat: %w", err)
			}
		}
	}
}

func writeMsg(conn *websocket.Conn, msg wire.Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	return conn.WriteMessage(websocket.TextMessage, data)
}
