package proto

import (
	"encoding/json"
	"maps"
	"testing"
)

func TestMergeEnvOrder(t *testing.T) {
	template := map[string]string{"A": "template", "B": "template", "C": "template", "D": "template"}
	environment := map[string]string{"B": "environment", "C": "environment", "D": "environment"}
	create := map[string]string{"C": "create", "D": "create"}
	exec := map[string]string{"D": "exec"}
	got := MergeEnv(template, environment, create, exec)
	want := map[string]string{"A": "template", "B": "environment", "C": "create", "D": "exec"}
	if !maps.Equal(got, want) {
		t.Errorf("MergeEnv = %v, want %v", got, want)
	}
	if template["B"] != "template" {
		t.Error("MergeEnv changed its first layer")
	}
}

func TestFileModeJSON(t *testing.T) {
	b, _ := json.Marshal(FileMode(0o640))
	if string(b) != `"0640"` {
		t.Errorf("marshal = %s", b)
	}
	var m FileMode
	if err := json.Unmarshal([]byte(`"0600"`), &m); err != nil || m != 0o600 {
		t.Errorf("unmarshal = %o, %v", m, err)
	}
	for _, bad := range []string{`"1777"`, `"rw"`, `384`} {
		if json.Unmarshal([]byte(bad), &m) == nil {
			t.Errorf("unmarshal %s succeeded", bad)
		}
	}
}
