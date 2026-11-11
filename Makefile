VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Pi to deploy the agent to: make deploy-agent PI=user@host
PI ?= birdcam@raspberrypi.local
AGENT_LDFLAGS := -s -w -X main.version=$(VERSION)
SERVER_LDFLAGS := -s -w -X birdcam/internal/server.Version=$(VERSION)
IMAGE ?= ghcr.io/nbokdam/birdcam-server

.PHONY: build build-agent build-server image deploy-agent test lint

build: build-agent build-server

build-agent:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(AGENT_LDFLAGS)" -o bin/birdcam-agent-linux-arm64 ./cmd/birdcam-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags "$(AGENT_LDFLAGS)" -o bin/birdcam-agent-linux-armv7 ./cmd/birdcam-agent

build-server:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(SERVER_LDFLAGS)" -o bin/birdcam-server ./cmd/birdcam-server

# Local container image of the server (CI builds and pushes multi-arch images).
image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):dev .

deploy-agent: bin/birdcam-agent-linux-arm64
	ssh -o BatchMode=yes $(PI) 'mkdir -p ~/birdcam-agent-deploy'
	scp -o BatchMode=yes bin/birdcam-agent-linux-arm64 $(PI):birdcam-agent-deploy/birdcam-agent
	scp -o BatchMode=yes deploy/birdcam-agent.service deploy/agent.example.yaml deploy/install-agent.sh $(PI):birdcam-agent-deploy/
	ssh -o BatchMode=yes -t $(PI) 'sudo sh ~/birdcam-agent-deploy/install-agent.sh'

bin/birdcam-agent-linux-arm64:
	$(MAKE) build-agent

test:
	go test ./...
	node --test web/test

lint:
	test -z "$$(gofmt -l .)"
	go vet ./...
