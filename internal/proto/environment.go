package proto

import (
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
)

// NoEnvironment as CreateRequest.Environment starts a sandbox without one, even when the
// base environment exists.
const NoEnvironment = "none"

// BaseEnvironment is the environment a sandbox starts with when the request names none.
const BaseEnvironment = "base"

// Environment is what the API shows of a stored environment. Values and file contents never
// leave sbxd, so it has only names.
type Environment struct {
	Name  string            `json:"name"`
	Vars  []string          `json:"vars"`
	Files []EnvironmentPath `json:"files"`
}

type EnvironmentPath struct {
	Path string   `json:"path"`
	Mode FileMode `json:"mode"`
}

// EnvironmentUpdate is the body of PUT /v1/environments/{name}. It creates the environment
// or merges into it like a JSON merge patch: a null var or file removes it.
type EnvironmentUpdate struct {
	Vars map[string]*string `json:"vars,omitempty"`
	// Files is keyed by absolute guest path. The files belong to DefaultUser.
	Files map[string]*EnvironmentFile `json:"files,omitempty"`
}

type EnvironmentFile struct {
	Content []byte `json:"content"`
	// Mode defaults to 0600.
	Mode FileMode `json:"mode,omitempty"`
}

// FileMode is a permission mode that travels as an octal string such as "0600".
type FileMode uint32

func (m FileMode) MarshalJSON() ([]byte, error) {
	return json.Marshal(fmt.Sprintf("%04o", uint32(m)))
}

func (m *FileMode) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("mode must be an octal string like \"0600\": %w", err)
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil || v > 0o777 {
		return fmt.Errorf("mode %q must be octal permission bits such as \"0600\"", s)
	}
	*m = FileMode(v)
	return nil
}

// MergeEnv layers environments, later ones winning. A command sees, from weakest to
// strongest, the agent's defaults, the sandbox's environment, the create request's env,
// and the exec request's env.
func MergeEnv(layers ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, l := range layers {
		maps.Copy(out, l)
	}
	return out
}
