//go:build linux

// Package daemon is sbxd: it restores sandboxes from template snapshots with Firecracker,
// gives each one an isolated network, and proxies the sbx API to the agent inside.
package daemon

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

type Config struct {
	Listen string `json:"listen"`
	// PublicHost is the address put in URLs of exposed ports. Empty means the host name the
	// client used to reach the API, which is right behind a port forward.
	PublicHost   string         `json:"public_host"`
	DataDir      string         `json:"data_dir"`
	TokenFile    string         `json:"token_file"`
	Firecracker  string         `json:"firecracker"`
	Jailer       string         `json:"jailer"`
	Kernel       string         `json:"kernel"`
	AgentBinary  string         `json:"agent_binary"`
	MaxSandboxes int            `json:"max_sandboxes"`
	PoolSize     int            `json:"pool_size"`
	PortMin      int            `json:"port_min"`
	PortMax      int            `json:"port_max"`
	DefaultTTL   proto.Duration `json:"default_ttl"`
	// MinFreeMiB refuses new sandboxes when the host has less memory available.
	MinFreeMiB int `json:"min_free_mib"`
	// UIDBase + slot is the unprivileged user each jailed Firecracker runs as.
	UIDBase int `json:"uid_base"`
	// EgressPort is where restricted sandboxes' DNS, HTTP, and HTTPS arrive, on TCP and UDP.
	EgressPort int `json:"egress_port"`
}

func LoadConfig(path string) (*Config, error) {
	c := &Config{
		Listen:       ":7070",
		DataDir:      "/var/lib/sbx",
		TokenFile:    "/etc/sbx/token",
		Firecracker:  "/usr/local/bin/firecracker",
		Jailer:       "/usr/local/bin/jailer",
		Kernel:       "/var/lib/sbx/kernel/vmlinux",
		AgentBinary:  "/usr/local/lib/sbx/sbx-agent",
		MaxSandboxes: 64,
		PoolSize:     8,
		PortMin:      30000,
		PortMax:      30999,
		DefaultTTL:   proto.Duration(time.Hour),
		MinFreeMiB:   512,
		UIDBase:      200000,
		EgressPort:   7071,
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	return c, json.Unmarshal(b, c)
}
