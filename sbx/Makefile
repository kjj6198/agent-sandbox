VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto.Version=$(VERSION)
DIST := dist

.PHONY: all cli dist clean

all: cli dist

# sbx for this machine.
cli:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/sbx ./cmd/sbx

# Everything install.sh needs on the sbx host.
dist:
	rm -rf $(DIST) && mkdir -p $(DIST)/bin
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/bin/ ./cmd/sbxd ./cmd/sbx ./cmd/sbx-agent
	cp deploy/install.sh deploy/sbxd.service $(DIST)/
	cp -R kernel templates $(DIST)/

clean:
	rm -rf bin $(DIST)
