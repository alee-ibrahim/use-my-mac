VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w
GO ?= go

.PHONY: all build test vet web agent-darwin dist-mac docker run-dev clean

all: vet test build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS) -X main.version=$(VERSION)" -o bin/ ./cmd/...

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...
	GOOS=darwin GOARCH=arm64 $(GO) vet ./...

web:
	cd dashboard && npm ci && npm run build

# The agent binary for Apple Silicon Macs (pure Go, no cgo needed).
agent-darwin:
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath \
		-ldflags "$(LDFLAGS) -X github.com/alee-ibrahim/use-my-mac/internal/agent.Version=$(VERSION)" \
		-o dist/umm-agent-darwin-arm64/umm-agent ./cmd/umm-agent

# Folder + tarball to copy to a Mac: binary, install script, launchd plist.
dist-mac: agent-darwin
	cp deploy/mac/install.sh deploy/mac/uninstall.sh deploy/mac/agent.example.yaml \
		deploy/mac/io.github.alee-ibrahim.umm-agent.plist \
		deploy/mac/io.github.alee-ibrahim.umm-agent.updater.plist dist/umm-agent-darwin-arm64/
	tar -C dist -czf dist/umm-agent-darwin-arm64.tar.gz umm-agent-darwin-arm64

docker:
	docker build --build-arg VERSION=$(VERSION) -t use-my-mac:$(VERSION) .

# Local router with SQLite and local storage on :8080 (dashboard from dashboard/dist).
run-dev: build
	mkdir -p data
	UMM_PUBLIC_URL=http://localhost:8080 ./bin/umm-server -config deploy/dev.yaml

clean:
	rm -rf bin dist data
