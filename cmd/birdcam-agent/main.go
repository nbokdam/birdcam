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
	xuCam := flag.String("xu-bitrate", "", "debug: print native bitrate control (XU unit, current bitrate) of the given camera id and exit")
	xuSet := flag.Int("xu-set-bps", 0, "debug: with -xu-bitrate, also set the camera's H.264 bitrate (bit/s) and print the read-back")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if *xuCam != "" {
		for _, c := range agent.NewDiscoverer().Discover() {
			if c.ID != *xuCam {
				continue
			}
			if !c.NativeBitrateControl {
				fmt.Println("no control")
				return
			}
			fmt.Printf("xu unit %d node %s bitrate %d\n", c.XUUnit, c.XUNode, c.XUBitrateBps)
			if *xuSet > 0 {
				got, err := agent.ApplyBitrate(c.XUNode, c.XUUnit, *xuSet)
				fmt.Printf("set %d -> read back %d err %v\n", *xuSet, got, err)
			}
			return
		}
		fmt.Println("unknown camera")
		os.Exit(1)
	}
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
