// Command birdcam-agent is the Raspberry Pi side of BirdCam v2.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"birdcam/internal/agent"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfgPath := flag.String("config", "/etc/birdcam/agent.yaml", "path to agent config (YAML)")
	showVersion := flag.Bool("version", false, "print version and exit")
	listCams := flag.Bool("list-cameras", false, "print discovered cameras and hardware as JSON and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if *listCams {
		hw := agent.DetectHardware("/sys", "/proc/device-tree/model", 0)
		cams := agent.NewDiscoverer().Discover()
		out := map[string]any{"version": version, "hardware": hw, "cameras": cams}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			log.Error("encode", "err", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := agent.LoadConfig(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	agent.New(cfg, version, log).Run(ctx)
	log.Info("agent stopped")
}
