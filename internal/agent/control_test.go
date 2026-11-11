package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"birdcam/internal/protocol"
)

type recHandler struct {
	mu          sync.Mutex
	starts      []protocol.Start
	stops       []protocol.Stop
	fps         []string
	disconnects atomic.Int32
}

func (h *recHandler) OnStart(s protocol.Start, fp string) {
	h.fps = append(h.fps, fp)
	h.mu.Lock()
	h.starts = append(h.starts, s)
	h.mu.Unlock()
}
func (h *recHandler) OnStop(s protocol.Stop) {
	h.mu.Lock()
	h.stops = append(h.stops, s)
	h.mu.Unlock()
}
func (h *recHandler) OnDisconnect() { h.disconnects.Add(1) }

type srvConn struct {
	conn  *websocket.Conn
	hello protocol.Hello
}

// fakeServer accepts /agent, handles hello->welcome and then hands the conn to behave.
func fakeServer(t *testing.T, behave func(ctx context.Context, sc *srvConn)) (url string, conns chan *srvConn) {
	conns = make(chan *srvConn, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var h protocol.Hello
		if err := json.Unmarshal(data, &h); err != nil {
			return
		}
		out, _ := protocol.Encode(protocol.TypeWelcome, protocol.Welcome{ServerVersion: "test", IngestCertSHA256: "abc123"})
		if err := c.Write(ctx, websocket.MessageText, out); err != nil {
			return
		}
		sc := &srvConn{conn: c, hello: h}
		conns <- sc
		behave(ctx, sc)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/agent", conns
}

func newTestClient(url string, h Handler) *Client {
	return &Client{
		URL: url, Handler: h, Log: slog.New(slog.DiscardHandler),
		Hello: func() protocol.Hello {
			return protocol.Hello{ProtocolVersion: protocol.Version, DeviceID: "pi1", Key: "k", AgentVersion: "t"}
		},
		PingInterval: 50 * time.Millisecond, DeadAfter: 300 * time.Millisecond,
		NewBackoff: func() *Backoff { return NewBackoffSteps(20 * time.Millisecond) },
	}
}

func recv(t *testing.T, c *websocket.Conn, ctx context.Context) protocol.Envelope {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, data, err := c.Read(rctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var e protocol.Envelope
	_ = json.Unmarshal(data, &e)
	return e
}

func TestControlHelloPingStartAndServerDisconnect(t *testing.T) {
	var first atomic.Bool
	url, conns := fakeServer(t, func(ctx context.Context, sc *srvConn) {
		if first.CompareAndSwap(false, true) {
			// answer pings, send ping and a start, then hang up.
			msg, _ := protocol.Encode(protocol.TypePing, protocol.Ping{T: 77})
			_ = sc.conn.Write(ctx, websocket.MessageText, msg)
			st, _ := protocol.Encode(protocol.TypeStart, protocol.Start{CameraID: "c", SessionID: "s", Port: 9001, Token: "t"})
			_ = sc.conn.Write(ctx, websocket.MessageText, st)
			gotPong, gotPing := false, false
			for !(gotPong && gotPing) {
				_, data, err := sc.conn.Read(ctx)
				if err != nil {
					return
				}
				var e protocol.Envelope
				_ = json.Unmarshal(data, &e)
				switch e.Type {
				case protocol.TypePong:
					var p protocol.Pong
					_ = json.Unmarshal(data, &p)
					if p.T != 77 {
						t.Errorf("pong echo %d", p.T)
					}
					gotPong = true
				case protocol.TypePing:
					gotPing = true
				}
			}
			sc.conn.Close(websocket.StatusGoingAway, "bye")
			return
		}
		// second connection: keep it alive by pinging.
		for ctx.Err() == nil {
			msg, _ := protocol.Encode(protocol.TypePing, protocol.Ping{})
			if sc.conn.Write(ctx, websocket.MessageText, msg) != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	h := &recHandler{}
	c := newTestClient(url, h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	sc1 := <-conns
	if sc1.hello.DeviceID != "pi1" || sc1.hello.ProtocolVersion != protocol.Version {
		t.Errorf("hello %+v", sc1.hello)
	}
	select {
	case <-conns: // reconnected after server hang-up
	case <-time.After(3 * time.Second):
		t.Fatal("no reconnect")
	}
	h.mu.Lock()
	if len(h.starts) != 1 || h.starts[0].Port != 9001 {
		t.Errorf("starts %+v", h.starts)
	}
	h.mu.Unlock()
	if len(h.fps) != 1 || h.fps[0] != "abc123" {
		t.Errorf("fingerprint passed to OnStart: %v", h.fps)
	}
	if h.disconnects.Load() < 1 {
		t.Error("OnDisconnect not called")
	}
	var sendErr error
	for i := 0; i < 100; i++ {
		if sendErr = c.Send(protocol.TypeStreamStatus, protocol.StreamStatus{CameraID: "c", State: protocol.StateRunning}); sendErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sendErr != nil {
		t.Errorf("send: %v", sendErr)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestControlMissingPingsTriggerReconnect(t *testing.T) {
	// The server never sends anything after welcome.
	url, conns := fakeServer(t, func(ctx context.Context, sc *srvConn) {
		for {
			if _, _, err := sc.conn.Read(ctx); err != nil {
				return
			}
		}
	})
	h := &recHandler{}
	c := newTestClient(url, h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	<-conns
	start := time.Now()
	select {
	case <-conns:
	case <-time.After(3 * time.Second):
		t.Fatal("no reconnect after silence")
	}
	if d := time.Since(start); d < c.DeadAfter-50*time.Millisecond || d > c.DeadAfter+time.Second {
		t.Errorf("reconnected after %v, DeadAfter %v", d, c.DeadAfter)
	}
}

func TestControlErrorOnBadKey(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		attempts.Add(1)
		_, _, _ = c.Read(r.Context())
		out, _ := protocol.Encode(protocol.TypeError, protocol.Error{Message: "bad key"})
		_ = c.Write(r.Context(), websocket.MessageText, out)
		c.Close(websocket.StatusPolicyViolation, "bad key")
	}))
	defer srv.Close()
	c := newTestClient("ws"+strings.TrimPrefix(srv.URL, "http"), &recHandler{})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	c.Run(ctx)
	if attempts.Load() < 2 {
		t.Errorf("expected retries, got %d attempts", attempts.Load())
	}
}

func TestControlHelloTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		<-r.Context().Done() // never welcome
		c.CloseNow()
	}))
	defer srv.Close()
	c := newTestClient("ws"+strings.TrimPrefix(srv.URL, "http"), &recHandler{})
	c.defaults()
	c.HelloTimeout = 100 * time.Millisecond
	err := c.session(context.Background())
	if err == nil || !strings.Contains(err.Error(), "welcome") {
		t.Errorf("err = %v", err)
	}
}
