package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"

	"birdcam/internal/protocol"
)

// Handler receives server commands from the control connection.
type Handler interface {
	// OnStart gets the ingest cert fingerprint (may be empty) announced in
	// the Welcome of the control connection the start arrived on.
	OnStart(s protocol.Start, ingestCertSHA256 string)
	OnStop(protocol.Stop)
	// OnDisconnect is called whenever the control connection is lost.
	OnDisconnect()
}

// WelcomeHandler is optionally implemented by a Handler that wants the
// Welcome of every new control connection (e.g. to act on an update offer).
type WelcomeHandler interface {
	OnWelcome(protocol.Welcome)
}

// Client maintains the control WebSocket to the server.
type Client struct {
	URL     string
	Hello   func() protocol.Hello // called on every (re)connect
	Handler Handler
	Log     *slog.Logger

	// Tunables; zero values use the protocol defaults.
	PingInterval time.Duration
	DeadAfter    time.Duration
	DialTimeout  time.Duration
	HelloTimeout time.Duration
	WriteTimeout time.Duration
	SurviveReset time.Duration // a connection older than this resets the backoff (60s)
	NewBackoff   func() *Backoff
	connMu       sync.Mutex
	conn         *websocket.Conn
	writeMu      sync.Mutex
}

var errDead = errors.New("no message from server within deadline")

func (c *Client) defaults() {
	if c.PingInterval == 0 {
		c.PingInterval = protocol.PingInterval
	}
	if c.DeadAfter == 0 {
		c.DeadAfter = protocol.DeadAfter
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = 10 * time.Second
	}
	if c.HelloTimeout == 0 {
		c.HelloTimeout = 10 * time.Second
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if c.SurviveReset == 0 {
		c.SurviveReset = 60 * time.Second
	}
	if c.NewBackoff == nil {
		c.NewBackoff = NewBackoff
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
}

// Run connects and reconnects until ctx is cancelled.
func (c *Client) Run(ctx context.Context) {
	c.defaults()
	b := c.NewBackoff()
	for ctx.Err() == nil {
		start := time.Now()
		err := c.session(ctx)
		c.Handler.OnDisconnect()
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) >= c.SurviveReset {
			b.Reset()
		}
		d := b.Next()
		c.Log.Warn("control connection lost", "err", err, "reconnect_in", d)
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
	}
}

// Send sends a message on the current connection (serialized with all other writes).
func (c *Client) Send(msgType string, msg any) error {
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn == nil {
		return errors.New("not connected")
	}
	return c.write(conn, msgType, msg)
}

func (c *Client) write(conn *websocket.Conn, msgType string, msg any) error {
	data, err := protocol.Encode(msgType, msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), c.WriteTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}

func (c *Client) session(ctx context.Context) error {
	dctx, dcancel := context.WithTimeout(ctx, c.DialTimeout)
	conn, _, err := websocket.Dial(dctx, c.URL, nil)
	dcancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	if err := c.write(conn, protocol.TypeHello, c.Hello()); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	welcome, err := c.awaitWelcome(ctx, conn)
	if err != nil {
		return err
	}
	fp := welcome.IngestCertSHA256
	c.Log.Info("connected to server", "url", c.URL)
	if wh, ok := c.Handler.(WelcomeHandler); ok {
		wh.OnWelcome(welcome)
	}

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()
	defer func() {
		c.connMu.Lock()
		c.conn = nil
		c.connMu.Unlock()
	}()

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	in := make(chan []byte)
	errc := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.Read(sctx)
			if err != nil {
				errc <- err
				return
			}
			select {
			case in <- data:
			case <-sctx.Done():
				errc <- sctx.Err()
				return
			}
		}
	}()

	timer := time.NewTimer(c.DeadAfter)
	defer timer.Stop()
	ticker := time.NewTicker(c.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "shutdown")
			return ctx.Err()
		case <-timer.C:
			return errDead
		case <-ticker.C:
			if err := c.write(conn, protocol.TypePing, protocol.Ping{T: time.Now().UnixMilli()}); err != nil {
				return fmt.Errorf("send ping: %w", err)
			}
		case err := <-errc:
			return err
		case data := <-in:
			timer.Reset(c.DeadAfter)
			if err := c.dispatch(conn, data, fp); err != nil {
				return err
			}
		}
	}
}

func (c *Client) awaitWelcome(ctx context.Context, conn *websocket.Conn) (protocol.Welcome, error) {
	hctx, cancel := context.WithTimeout(ctx, c.HelloTimeout)
	defer cancel()
	for {
		_, data, err := conn.Read(hctx)
		if err != nil {
			return protocol.Welcome{}, fmt.Errorf("waiting for welcome: %w", err)
		}
		var env protocol.Envelope
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		switch env.Type {
		case protocol.TypeWelcome:
			var w protocol.Welcome
			_ = json.Unmarshal(data, &w)
			return w, nil
		case protocol.TypeError:
			var e protocol.Error
			_ = json.Unmarshal(data, &e)
			return protocol.Welcome{}, fmt.Errorf("server rejected hello: %s", e.Message)
		case protocol.TypePing:
			var p protocol.Ping
			_ = json.Unmarshal(data, &p)
			_ = c.write(conn, protocol.TypePong, protocol.Pong{T: p.T})
		}
	}
}

func (c *Client) dispatch(conn *websocket.Conn, data []byte, ingestFP string) error {
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		c.Log.Warn("bad message from server", "err", err)
		return nil
	}
	switch env.Type {
	case protocol.TypePing:
		var p protocol.Ping
		_ = json.Unmarshal(data, &p)
		if err := c.write(conn, protocol.TypePong, protocol.Pong{T: p.T}); err != nil {
			return fmt.Errorf("send pong: %w", err)
		}
	case protocol.TypePong, protocol.TypeWelcome:
	case protocol.TypeStart:
		var s protocol.Start
		if err := json.Unmarshal(data, &s); err != nil {
			c.Log.Warn("bad start", "err", err)
			return nil
		}
		c.Handler.OnStart(s, ingestFP)
	case protocol.TypeStop:
		var s protocol.Stop
		if err := json.Unmarshal(data, &s); err != nil {
			c.Log.Warn("bad stop", "err", err)
			return nil
		}
		c.Handler.OnStop(s)
	case protocol.TypeError:
		var e protocol.Error
		_ = json.Unmarshal(data, &e)
		c.Log.Error("server error", "message", e.Message)
	default:
		c.Log.Warn("unknown message type", "type", env.Type)
	}
	return nil
}
