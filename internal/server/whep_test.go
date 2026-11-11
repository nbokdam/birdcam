package server

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"

	"birdcam/internal/fakeagent"
	"birdcam/internal/protocol"
	"birdcam/internal/server/media"
)

// testViewer is a pion WebRTC client that watches a camera via WHEP.
type testViewer struct {
	pc       *webrtc.PeerConnection
	location string
	video    atomic.Int64
	audio    atomic.Int64
	videoPT  atomic.Value
	mu       sync.Mutex
	firstPay []byte
	codec    string
}

func newTestViewer(t *testing.T, baseURL, dev, cam string) *testViewer {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		t.Fatal(err)
	}
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	tv := &testViewer{pc: pc}
	t.Cleanup(func() { pc.Close() })
	for _, k := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := pc.AddTransceiverFromKind(k, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatal(err)
		}
	}
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			p, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			if tr.Kind() == webrtc.RTPCodecTypeVideo {
				if tv.video.Add(1) == 1 {
					tv.mu.Lock()
					tv.firstPay = append([]byte(nil), p.Payload...)
					tv.codec = tr.Codec().MimeType + " " + tr.Codec().SDPFmtpLine
					tv.mu.Unlock()
				}
			} else {
				tv.audio.Add(1)
			}
		}
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	resp, err := http.Post(baseURL+"/api/cameras/"+dev+"/"+cam+"/whep", "application/sdp",
		strings.NewReader(pc.LocalDescription().SDP))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("WHEP POST: %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/sdp" {
		t.Fatalf("answer content type %q", ct)
	}
	tv.location = resp.Header.Get("Location")
	if !strings.HasPrefix(tv.location, "/api/viewers/") {
		t.Fatalf("Location = %q", tv.location)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(body)}); err != nil {
		t.Fatal(err)
	}
	return tv
}

// TestEndToEndWebRTC: fake agent → ingest → server → WebRTC → pion viewer.
func TestEndToEndWebRTC(t *testing.T) {
	e := newTestEnv(t, 2, true)
	cfg := fakeagent.Config{}
	if path := os.Getenv("BIRDCAM_H264"); path != "" {
		cfg.NewSource = func() (fakeagent.FrameSource, error) { return fakeagent.FileSource(path) }
		t.Logf("streaming %s", path)
	}
	a, _, _ := e.runAgent(cfg)
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	waitFor(t, "camera", 2*time.Second, func() bool { _, ok := e.camera("dev1", "fakecam"); return ok })

	tv := newTestViewer(t, e.hs.URL, "dev1", "fakecam")
	waitEvent(t, a, protocol.TypeStart, 2*time.Second)
	waitFor(t, "video+audio RTP at the viewer", 10*time.Second, func() bool {
		return tv.video.Load() >= 60 && tv.audio.Load() >= 20
	})
	tv.mu.Lock()
	info := media.ParseH264(tv.firstPay)
	t.Logf("viewer received %d video / %d audio packets; negotiated %s", tv.video.Load(), tv.audio.Load(), tv.codec)
	if info.SPS == nil && !info.IDRStart {
		t.Errorf("first video packet is not a keyframe start: %x", tv.firstPay[:min(8, len(tv.firstPay))])
	}
	if !strings.Contains(tv.codec, "packetization-mode=1") {
		t.Errorf("negotiated codec %q", tv.codec)
	}
	tv.mu.Unlock()
	if c, _ := e.camera("dev1", "fakecam"); c.Viewers != 1 || !c.Streaming {
		t.Fatalf("camera: %+v", c)
	}

	// Ending the viewer via DELETE stops the stream after the linger.
	req, _ := http.NewRequest(http.MethodDelete, e.hs.URL+tv.location, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE: %d", resp.StatusCode)
	}
	waitFor(t, "viewer count 0", 2*time.Second, func() bool { c, _ := e.camera("dev1", "fakecam"); return c.Viewers == 0 })
	waitEvent(t, a, protocol.TypeStop, 2*time.Second)
}

// TestViewerPeerConnectionClosed: a viewer that just goes away (no DELETE)
// is removed once its PeerConnection closes/fails.
func TestViewerPeerConnectionClosed(t *testing.T) {
	e := newTestEnv(t, 2, true)
	a, _, _ := e.runAgent(fakeagent.Config{})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	waitFor(t, "camera", 2*time.Second, func() bool { _, ok := e.camera("dev1", "fakecam"); return ok })
	tv := newTestViewer(t, e.hs.URL, "dev1", "fakecam")
	waitFor(t, "video", 10*time.Second, func() bool { return tv.video.Load() > 10 })
	tv.pc.Close() // sends DTLS close_notify → server PC closes
	waitFor(t, "viewer removed", 10*time.Second, func() bool { c, _ := e.camera("dev1", "fakecam"); return c.Viewers == 0 })
	waitEvent(t, a, protocol.TypeStop, 2*time.Second)
}

func TestWHEPErrors(t *testing.T) {
	e := newTestEnv(t, 1, true)
	resp, _ := http.Post(e.hs.URL+"/api/cameras/dev1/nope/whep", "application/sdp", strings.NewReader("v=0"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown camera: %d", resp.StatusCode)
	}
	resp, _ = http.Post(e.hs.URL+"/api/cameras/dev1/nope/whep", "text/plain", strings.NewReader("v=0"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("content type: %d", resp.StatusCode)
	}
	a, _, _ := e.runAgent(fakeagent.Config{NoStream: true})
	waitEvent(t, a, protocol.TypeWelcome, 2*time.Second)
	waitFor(t, "camera", 2*time.Second, func() bool { _, ok := e.camera("dev1", "fakecam"); return ok })
	resp, _ = http.Post(e.hs.URL+"/api/cameras/dev1/fakecam/whep", "application/sdp", strings.NewReader("garbage"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad sdp: %d", resp.StatusCode)
	}
	// The failed negotiation must not leave demand behind.
	if c, _ := e.camera("dev1", "fakecam"); c.Viewers != 0 {
		t.Fatalf("viewers after failed negotiation: %d", c.Viewers)
	}
	req, _ := http.NewRequest(http.MethodDelete, e.hs.URL+"/api/viewers/doesnotexist", nil)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete unknown viewer: %d", resp.StatusCode)
	}
}

func TestStaticAndAPI(t *testing.T) {
	e := newTestEnv(t, 1, false)
	for path, want := range map[string]string{"/": "<video", "/app.js": "whep", "/style.css": "#overlay", "/api/cameras": "[]"} {
		resp, err := http.Get(e.hs.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), want) {
			t.Errorf("%s: %d, missing %q", path, resp.StatusCode, want)
		}
	}
}

func TestAgentProtocolErrors(t *testing.T) {
	e := newTestEnv(t, 1, false)
	ctx := context.Background()

	// Non-hello first message → error + close.
	ws, _, err := websocket.Dial(ctx, e.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := protocol.Encode(protocol.TypePing, protocol.Ping{T: 1})
	ws.Write(ctx, websocket.MessageText, b)
	_, data, err := ws.Read(ctx)
	if err != nil || !strings.Contains(string(data), `"type":"error"`) {
		t.Fatalf("want error message, got %s %v", data, err)
	}
	if _, _, err := ws.Read(ctx); err == nil {
		t.Fatal("connection should be closed")
	}

	// Silent client → closed after the hello timeout.
	ws, _, err = websocket.Dial(ctx, e.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, err := ws.Read(rctx); err == nil || rctx.Err() != nil {
		t.Fatalf("expected server close, got %v", err)
	}
	if d := time.Since(start); d < time.Second {
		t.Fatalf("closed too early: %v", d)
	}

	// Authenticated agent that stops answering → closed after DeadAfter.
	ws, _, err = websocket.Dial(ctx, e.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = protocol.Encode(protocol.TypeHello, protocol.Hello{ProtocolVersion: protocol.Version, DeviceID: "dev2", Key: "key2"})
	ws.Write(ctx, websocket.MessageText, b)
	var gotPing bool
	start = time.Now()
	for {
		_, data, err := ws.Read(rctx)
		if err != nil {
			break
		}
		if strings.Contains(string(data), `"type":"ping"`) {
			gotPing = true
		}
	}
	if !gotPing {
		t.Error("server did not ping")
	}
	if d := time.Since(start); d < 1500*time.Millisecond || rctx.Err() != nil {
		t.Fatalf("dead agent closed after %v (ctx err %v)", d, rctx.Err())
	}
}
