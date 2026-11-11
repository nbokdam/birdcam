// Command birdcam-probe is a headless WebRTC viewer for testing: it watches a
// camera through the server's WHEP endpoint for a while and reports what
// arrived. Optionally writes the received H.264 (Annex-B) and Opus (Ogg).
//
//	birdcam-probe -server http://localhost:8080 -device test-pi -camera <id> -duration 30s -h264 out.h264 -ogg out.ogg
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/h264writer"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"

	"birdcam/internal/server/media"
)

type stats struct {
	mu                 sync.Mutex
	vPkts, aPkts       int
	vBytes, aBytes     int
	idrs               int
	firstVideo, firstI time.Duration
	codecV, codecA     string
}

func main() {
	server := flag.String("server", "http://localhost:8080", "server base URL")
	device := flag.String("device", "", "device id")
	camera := flag.String("camera", "", "camera id")
	dur := flag.Duration("duration", 20*time.Second, "how long to watch")
	h264Out := flag.String("h264", "", "write received H.264 to this file")
	oggOut := flag.String("ogg", "", "write received Opus to this Ogg file")
	flag.Parse()
	if *device == "" || *camera == "" {
		fmt.Fprintln(os.Stderr, "need -device and -camera (see /api/cameras)")
		os.Exit(2)
	}
	if err := run(*server, *device, *camera, *dur, *h264Out, *oggOut); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(server, device, camera string, dur time.Duration, h264Out, oggOut string) error {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return err
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}
	defer pc.Close()
	for _, k := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := pc.AddTransceiverFromKind(k, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			return err
		}
	}

	var hw *h264writer.H264Writer
	if h264Out != "" {
		if hw, err = h264writer.New(h264Out); err != nil {
			return err
		}
		defer hw.Close()
	}
	var ow *oggwriter.OggWriter
	if oggOut != "" {
		if ow, err = oggwriter.New(oggOut, 48000, 2); err != nil {
			return err
		}
		defer ow.Close()
	}

	st := &stats{}
	t0 := time.Now()
	var wmu sync.Mutex
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		video := tr.Kind() == webrtc.RTPCodecTypeVideo
		st.mu.Lock()
		if video {
			st.codecV = tr.Codec().MimeType + " " + tr.Codec().SDPFmtpLine
		} else {
			st.codecA = tr.Codec().MimeType
		}
		st.mu.Unlock()
		for {
			p, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			record(st, p, video, time.Since(t0))
			wmu.Lock()
			if video && hw != nil {
				_ = hw.WriteRTP(p)
			}
			if !video && ow != nil {
				_ = ow.WriteRTP(p)
			}
			wmu.Unlock()
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("%6.2fs  peer connection: %s\n", time.Since(t0).Seconds(), s)
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	<-gathered
	resp, err := http.Post(server+"/api/cameras/"+device+"/"+camera+"/whep", "application/sdp",
		strings.NewReader(pc.LocalDescription().SDP))
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("WHEP POST: %d %s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	defer func() {
		req, _ := http.NewRequest(http.MethodDelete, server+loc, nil)
		if r, err := http.DefaultClient.Do(req); err == nil {
			r.Body.Close()
		}
	}()
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(body)}); err != nil {
		return err
	}

	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	end := time.After(dur)
	var lastV, lastA int
	for {
		select {
		case <-tick.C:
			st.mu.Lock()
			fmt.Printf("%6.2fs  video %d pkts %.0f kbps, audio %d pkts %.0f kbps, IDRs %d\n",
				time.Since(t0).Seconds(), st.vPkts, float64(st.vBytes-lastV)*8/5000, st.aPkts, float64(st.aBytes-lastA)*8/5000, st.idrs)
			lastV, lastA = st.vBytes, st.aBytes
			st.mu.Unlock()
		case <-end:
			st.mu.Lock()
			defer st.mu.Unlock()
			fmt.Printf("\nvideo codec: %s\naudio codec: %s\n", st.codecV, st.codecA)
			fmt.Printf("first video packet after %v, first IDR after %v\n", st.firstVideo.Round(time.Millisecond), st.firstI.Round(time.Millisecond))
			fmt.Printf("total: video %d pkts / %d KB, audio %d pkts / %d KB, IDRs %d\n", st.vPkts, st.vBytes/1024, st.aPkts, st.aBytes/1024, st.idrs)
			if st.vPkts == 0 {
				return fmt.Errorf("no video received")
			}
			return nil
		}
	}
}

func record(st *stats, p *rtp.Packet, video bool, at time.Duration) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !video {
		st.aPkts++
		st.aBytes += len(p.Payload)
		return
	}
	st.vPkts++
	st.vBytes += len(p.Payload)
	if st.vPkts == 1 {
		st.firstVideo = at
	}
	if media.ParseH264(p.Payload).IDRStart {
		st.idrs++
		if st.idrs == 1 {
			st.firstI = at
		}
	}
}
