// Package proto holds the wire types shared by the sbx CLI, the sbxd daemon, and the
// sbx-agent that runs inside every microVM.
package proto

import (
	"encoding/json"
	"fmt"
	"time"
)

// Version is stamped at build time with -ldflags "-X .../proto.Version=...".
var Version = "dev"

// AgentPort is the vsock port sbx-agent listens on inside the guest.
const AgentPort = 1024

// DefaultUser runs commands unless a request names another user. It has passwordless sudo.
const DefaultUser = "user"

// Duration is a time.Duration that travels as a Go duration string such as "30m".
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"30m\": %w", err)
	}
	v, err := ParseDuration(s)
	*d = Duration(v)
	return err
}

// ParseDuration accepts Go durations plus a bare "0" and day suffixes such as "2d".
func ParseDuration(s string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	var days int
	if n, err := fmt.Sscanf(s, "%dd", &days); n == 1 && err == nil && fmt.Sprintf("%dd", days) == s {
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

type Sandbox struct {
	ID        string    `json:"id"`
	Template  string    `json:"template"`
	CPUs      int       `json:"cpus"`
	MemoryMiB int       `json:"memory_mib"`
	CreatedAt time.Time `json:"created_at"`
	// TTL is an idle timeout: the sandbox is destroyed after TTL without API activity. 0 keeps it.
	TTL       Duration   `json:"ttl"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Ports     []Port     `json:"ports,omitempty"`
	// CreateMs is how long the restore took, from request to the guest agent answering.
	CreateMs float64 `json:"create_ms"`
	// MemoryUsedMiB is the memory only this sandbox holds: guest pages it wrote plus the VMM's
	// own. Pages it only read from the template are shared page cache and not counted.
	MemoryUsedMiB float64 `json:"memory_used_mib,omitempty"`
}

type Port struct {
	Port     int    `json:"port"`
	HostPort int    `json:"host_port"`
	URL      string `json:"url"`
}

type Template struct {
	Name      string    `json:"name"`
	CPUs      int       `json:"cpus"`
	MemoryMiB int       `json:"memory_mib"`
	DiskGiB   int       `json:"disk_gib"`
	Parent    string    `json:"parent,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// DiskBytes is the space the template's files take on the host.
	DiskBytes int64 `json:"disk_bytes"`
}

type CreateRequest struct {
	Template string `json:"template,omitempty"`
	// TTL overrides the daemon's default idle timeout. Nil uses the default, "0" never expires.
	TTL *Duration         `json:"ttl,omitempty"`
	Env map[string]string `json:"env,omitempty"`
}

type UpdateRequest struct {
	TTL *Duration `json:"ttl,omitempty"`
}

type ExposeRequest struct {
	Port int `json:"port"`
}

type SnapshotRequest struct {
	Name string `json:"name"`
}

type Status struct {
	Version      string   `json:"version"`
	Firecracker  string   `json:"firecracker"`
	Sandboxes    int      `json:"sandboxes"`
	MaxSandboxes int      `json:"max_sandboxes"`
	MemAvailMiB  int      `json:"mem_available_mib"`
	MemTotalMiB  int      `json:"mem_total_mib"`
	DiskFreeGiB  int      `json:"disk_free_gib"`
	Templates    int      `json:"templates"`
	PublicHost   string   `json:"public_host"`
	DefaultTTL   Duration `json:"default_ttl"`
	PoolReady    int      `json:"pool_ready"`
}

type Error struct {
	Error string `json:"error"`
}

// Screenshot describes one captured desktop image. The image bytes travel as the body of
// GET /v1/sandboxes/{id}/screenshot (with the size repeated in X-Screen-* headers); this
// shape is what `sbx screenshot --json` and the viewer's /screen.json report about it.
type Screenshot struct {
	ID     string `json:"id"`
	Path   string `json:"path,omitempty"`
	Format string `json:"format"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Bytes  int    `json:"bytes,omitempty"`
	// ScreenshotURL and ViewerURL are set by the viewer server for external renderers.
	ScreenshotURL string `json:"screenshot_url,omitempty"`
	ViewerURL     string `json:"viewer_url,omitempty"`
}

// InitRequest is what sbxd sends to the agent right after every boot or restore.
type InitRequest struct {
	// UnixNano is the host's wall clock. A restored guest otherwise keeps the snapshot's time.
	UnixNano int64             `json:"unix_nano"`
	Hostname string            `json:"hostname"`
	Env      map[string]string `json:"env,omitempty"`
}

type Health struct {
	Version string  `json:"version"`
	Uptime  float64 `json:"uptime"`
}
