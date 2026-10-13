VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto.Version=$(VERSION)
DIST := dist

.PHONY: all cli dist install clean

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

# Install on HOST over ssh, or on this machine without HOST. INSTALL_ENV passes settings to
# install.sh, such as INSTALL_ENV=SBX_DATA_DISK=/dev/sdb, and STEPS limits what runs.
install: dist
ifdef HOST
	COPYFILE_DISABLE=1 tar --no-xattrs -C $(DIST) -czf - . | ssh $(HOST) 'rm -rf sbx-dist && mkdir sbx-dist && tar -xzf - -C sbx-dist'
	ssh -t $(HOST) sudo $(INSTALL_ENV) sbx-dist/install.sh $(STEPS)
else
	sudo $(INSTALL_ENV) $(DIST)/install.sh $(STEPS)
endif

clean:
	rm -rf bin $(DIST)
