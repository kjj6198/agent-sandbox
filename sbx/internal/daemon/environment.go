//go:build linux

package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

// An environment is a named set of variables and files that new sandboxes start with, so
// clients do not have to hold the secrets themselves. Each is stored as
// <EnvironmentDir>/<name>.json, readable only by root, and its values never leave sbxd.
type environment struct {
	Vars  map[string]string                `json:"vars"`
	Files map[string]proto.EnvironmentFile `json:"files"`
}

const (
	maxEnvVars       = 128
	maxEnvValue      = 32 << 10
	maxEnvFiles      = 32
	maxEnvFileSize   = 256 << 10
	maxEnvPath       = 1024
	maxEnvUpdateBody = 16 << 20
)

var envVarRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

func validEnvName(name string) error {
	if !nameRE.MatchString(name) || name == proto.NoEnvironment {
		return invalid("environment name %q must be 1-32 lowercase letters, digits, or dashes, and not %q", name, proto.NoEnvironment)
	}
	return nil
}

func (e *environment) validate() error {
	if len(e.Vars) > maxEnvVars {
		return invalid("an environment holds at most %d variables", maxEnvVars)
	}
	for k, v := range e.Vars {
		if !envVarRE.MatchString(k) {
			return invalid("variable name %q must be letters, digits, and underscores, not starting with a digit", k)
		}
		if len(v) > maxEnvValue {
			return invalid("variable %s is longer than %d KiB", k, maxEnvValue>>10)
		}
	}
	if len(e.Files) > maxEnvFiles {
		return invalid("an environment holds at most %d files", maxEnvFiles)
	}
	for p, f := range e.Files {
		if len(p) > maxEnvPath || !path.IsAbs(p) || path.Clean(p) != p || p == "/" {
			return invalid("file path %q must be absolute and clean, without . or .. parts", p)
		}
		if len(f.Content) > maxEnvFileSize {
			return invalid("file %s is larger than %d KiB", p, maxEnvFileSize>>10)
		}
	}
	return nil
}

// apply merges u into e. A null var or file removes it.
func (e *environment) apply(u proto.EnvironmentUpdate) {
	for k, v := range u.Vars {
		if v == nil {
			delete(e.Vars, k)
		} else {
			e.Vars[k] = *v
		}
	}
	for p, f := range u.Files {
		switch {
		case f == nil:
			delete(e.Files, p)
		case f.Mode == 0:
			e.Files[p] = proto.EnvironmentFile{Content: f.Content, Mode: 0o600}
		default:
			e.Files[p] = *f
		}
	}
}

// info is the environment as the API shows it: names and paths, no values.
func (e *environment) info(name string) proto.Environment {
	out := proto.Environment{Name: name, Vars: slices.Sorted(maps.Keys(e.Vars)), Files: []proto.EnvironmentPath{}}
	for _, p := range slices.Sorted(maps.Keys(e.Files)) {
		out.Files = append(out.Files, proto.EnvironmentPath{Path: p, Mode: e.Files[p].Mode})
	}
	return out
}

func (d *Daemon) environmentPath(name string) string {
	return filepath.Join(d.cfg.EnvironmentDir, name+".json")
}

func (d *Daemon) environment(name string) (*environment, error) {
	if err := validEnvName(name); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(d.environmentPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("environment %q %w; see `sbx env ls`", name, errNotFound)
	}
	if err != nil {
		return nil, err
	}
	e := &environment{}
	if err := json.Unmarshal(b, e); err != nil {
		return nil, fmt.Errorf("environment %q: %w", name, err)
	}
	return e, nil
}

// createEnvironment resolves CreateRequest.Environment to the name a sandbox records and
// the environment it starts with. Both are empty when it starts with none.
func (d *Daemon) createEnvironment(name string) (string, *environment, error) {
	switch name {
	case proto.NoEnvironment:
		return "", &environment{}, nil
	case "":
		e, err := d.environment(proto.BaseEnvironment)
		if errors.Is(err, errNotFound) {
			return "", &environment{}, nil
		}
		return proto.BaseEnvironment, e, err
	}
	e, err := d.environment(name)
	return name, e, err
}

func (d *Daemon) Environments() []proto.Environment {
	files, _ := filepath.Glob(filepath.Join(d.cfg.EnvironmentDir, "*.json"))
	out := []proto.Environment{}
	for _, f := range files {
		name := filepath.Base(f[:len(f)-len(".json")])
		if e, err := d.environment(name); err == nil {
			out = append(out, e.info(name))
		}
	}
	return out
}

// UpdateEnvironment creates environment name or merges u into it. Running sandboxes keep
// what they started with.
func (d *Daemon) UpdateEnvironment(name string, u proto.EnvironmentUpdate) (proto.Environment, error) {
	if err := validEnvName(name); err != nil {
		return proto.Environment{}, err
	}
	d.envMu.Lock()
	defer d.envMu.Unlock()
	e, err := d.environment(name)
	if errors.Is(err, errNotFound) {
		e, err = &environment{}, nil
	}
	if err != nil {
		return proto.Environment{}, err
	}
	if e.Vars == nil {
		e.Vars = map[string]string{}
	}
	if e.Files == nil {
		e.Files = map[string]proto.EnvironmentFile{}
	}
	e.apply(u)
	if err := e.validate(); err != nil {
		return proto.Environment{}, err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return proto.Environment{}, err
	}
	if err := os.MkdirAll(d.cfg.EnvironmentDir, 0o700); err != nil {
		return proto.Environment{}, err
	}
	p := d.environmentPath(name)
	if err := os.WriteFile(p+".tmp", b, 0o600); err != nil {
		return proto.Environment{}, err
	}
	return e.info(name), os.Rename(p+".tmp", p)
}

func (d *Daemon) DeleteEnvironment(name string) error {
	if _, err := d.environment(name); err != nil {
		return err
	}
	d.envMu.Lock()
	defer d.envMu.Unlock()
	return os.Remove(d.environmentPath(name))
}
