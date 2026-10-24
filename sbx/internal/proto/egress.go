package proto

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Egress limits a sandbox to the hosts it names. A sandbox without one reaches the whole
// internet. Only DNS, HTTP on port 80, and HTTPS on port 443 pass, and only for allowed host
// names; everything else the guest sends is dropped.
type Egress struct {
	// Allow holds host names such as "github.com". "*.example.com" matches every subdomain
	// of example.com but not example.com itself. Empty allows nothing.
	Allow []string `json:"allow"`
}

// Blocked is a host the sandbox tried to reach outside its allowlist.
type Blocked struct {
	Host string    `json:"host"`
	At   time.Time `json:"at"` // the most recent attempt
}

// NewEgress validates and normalizes host patterns.
func NewEgress(hosts []string) (*Egress, error) {
	e := &Egress{Allow: []string{}}
	for _, h := range hosts {
		h = normalizeHost(h)
		name := strings.TrimPrefix(h, "*.")
		if name == "" || strings.ContainsAny(name, "*/:@ ") || !strings.Contains(name, ".") {
			return nil, fmt.Errorf("allow %q: use a host name such as github.com or *.github.com", h)
		}
		if !slices.Contains(e.Allow, h) {
			e.Allow = append(e.Allow, h)
		}
	}
	return e, nil
}

func (e *Egress) Allows(host string) bool {
	host = normalizeHost(host)
	for _, p := range e.Allow {
		if host == p || strings.HasPrefix(p, "*.") && strings.HasSuffix(host, p[1:]) {
			return true
		}
	}
	return false
}

func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}
