//go:build linux

package agent

import (
	"fmt"
	"os/user"
	"path/filepath"
	"strconv"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

type account struct {
	name     string
	uid, gid uint32
	groups   []uint32
	home     string
}

// lookupUser resolves a user name, defaulting to proto.DefaultUser. Images without that user
// fall back to root, so any OCI image works as a template.
func lookupUser(name string) (*account, error) {
	if name == "" {
		name = proto.DefaultUser
	}
	u, err := user.Lookup(name)
	if err != nil && name == proto.DefaultUser {
		u, err = user.Lookup("root")
	}
	if err != nil {
		return nil, fmt.Errorf("unknown user %q", name)
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	a := &account{name: u.Username, uid: uint32(uid), gid: uint32(gid), home: u.HomeDir}
	if a.home == "" {
		a.home = "/"
	}
	gids, _ := u.GroupIds()
	for _, g := range gids {
		if n, err := strconv.ParseUint(g, 10, 32); err == nil {
			a.groups = append(a.groups, uint32(n))
		}
	}
	return a, nil
}

// resolve makes a guest path absolute; relative paths are relative to the user's home.
func (a *account) resolve(p string) string {
	if p == "" {
		return a.home
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(a.home, p)
}
