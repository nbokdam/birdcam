VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PI ?= birdcam@192.168.11.115
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build-agent deploy-agent test

build-agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/birdcam-agent-linux-arm64 ./cmd/birdcam-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$(LDFLAGS)" -o bin/birdcam-agent-linux-armv7 ./cmd/birdcam-agent

# Usage: make deploy-agent PI=birdcam@192.168.11.115
deploy-agent: bin/birdcam-agent-linux-arm64
	ssh -o BatchMode=yes $(PI) 'mkdir -p ~/birdcam-agent-deploy'
	scp -o BatchMode=yes bin/birdcam-agent-linux-arm64 $(PI):birdcam-agent-deploy/birdcam-agent
	scp -o BatchMode=yes deploy/birdcam-agent.service deploy/agent.example.yaml deploy/install-agent.sh $(PI):birdcam-agent-deploy/
	ssh -o BatchMode=yes -t $(PI) 'sudo sh ~/birdcam-agent-deploy/install-agent.sh'

bin/birdcam-agent-linux-arm64:
	$(MAKE) build-agent

test:
	go test ./...
