// Command birdcam-fakeagent pretends to be a birdcam agent with one camera.
// On start it streams a raw H.264 Annex-B file (looped) plus silent Opus, so
// the server and web page can be tested without a Pi.
//
//	birdcam-fakeagent -server ws://localhost:8080/agent -device test -key secret -h264 capture.h264
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"birdcam/internal/fakeagent"
	"birdcam/internal/protocol"
)

func main() {
	serverURL := flag.String("server", "ws://localhost:8080/agent", "control WebSocket URL")
	device := flag.String("device", "fake", "device id")
	key := flag.String("key", "", "device key")
	camID := flag.String("camera", "fakecam", "camera id")
	camName := flag.String("name", "Fake camera", "camera name")
	h264 := flag.String("h264", "", "raw H.264 Annex-B file to loop (default: synthetic, not decodable)")
	fps := flag.Int("fps", 30, "frame rate of the H.264 file")
	audio := flag.Bool("audio", true, "advertise a microphone and send silent Opus")
	audioRate := flag.Float64("audio-rate", 1, "audio clock scale (!=1 simulates A/V drift)")
	ingestHost := flag.String("ingest-host", "", "ingest host (default: host of -server)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)

	cam := protocol.Camera{
		ID: *camID, Name: *camName, VideoDevice: "/dev/null",
		Formats: []protocol.VideoFormat{{Codec: "h264", Width: 1920, Height: 1080, FPS: []int{*fps}}},
	}
	if *audio {
		cam.AudioDevice = "hw:CARD=Fake,DEV=0"
	}
	cfg := fakeagent.Config{
		ServerURL: *serverURL, DeviceID: *device, Key: *key, IngestHost: *ingestHost,
		Cameras: []protocol.Camera{cam}, FPS: *fps, NoAudio: !*audio, AudioRate: *audioRate, Log: log,
	}
	if *h264 != "" {
		if _, err := fakeagent.FileSource(*h264); err != nil {
			log.Error("cannot use h264 file", "err", err)
			os.Exit(1)
		}
		path := *h264
		cfg.NewSource = func() (fakeagent.FrameSource, error) { return fakeagent.FileSource(path) }
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	backoff := []time.Duration{1, 2, 5, 10, 15}
	for attempt := 0; ; attempt++ {
		started := time.Now()
		err := fakeagent.New(cfg).Run(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, fakeagent.ErrRejected) {
			log.Error("server rejected agent", "err", err)
			os.Exit(1)
		}
		if time.Since(started) > time.Minute {
			attempt = 0
		}
		d := backoff[min(attempt, len(backoff)-1)] * time.Second
		log.Warn("disconnected, reconnecting", "err", err, "in", d)
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
	}
}
