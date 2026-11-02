//go:build linux

package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

func TestEnvironmentValidate(t *testing.T) {
	file := proto.EnvironmentFile{Content: []byte("x"), Mode: 0o600}
	for name, e := range map[string]environment{
		"digit first":   {Vars: map[string]string{"1A": ""}},
		"dash in name":  {Vars: map[string]string{"A-B": ""}},
		"long value":    {Vars: map[string]string{"A": strings.Repeat("x", maxEnvValue+1)}},
		"relative path": {Files: map[string]proto.EnvironmentFile{"home/user/a": file}},
		"dot dot":       {Files: map[string]proto.EnvironmentFile{"/home/user/../root/a": file}},
		"trailing /":    {Files: map[string]proto.EnvironmentFile{"/home/user/a/": file}},
		"root":          {Files: map[string]proto.EnvironmentFile{"/": file}},
		"large file":    {Files: map[string]proto.EnvironmentFile{"/a": {Content: make([]byte, maxEnvFileSize+1)}}},
	} {
		if err := e.validate(); err == nil {
			t.Errorf("%s: validate succeeded", name)
		}
	}
	ok := environment{
		Vars:  map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "secret", "_X": ""},
		Files: map[string]proto.EnvironmentFile{"/home/user/.codex/auth.json": file},
	}
	if err := ok.validate(); err != nil {
		t.Errorf("validate: %v", err)
	}
	for _, name := range []string{"", "none", "Base", "a_b", strings.Repeat("a", 33)} {
		if validEnvName(name) == nil {
			t.Errorf("validEnvName(%q) succeeded", name)
		}
	}
}

func testDaemon(t *testing.T) *Daemon {
	dir := t.TempDir()
	return &Daemon{cfg: &Config{EnvironmentDir: dir}, token: "t", sandboxes: map[string]*sandbox{}}
}

func apiCall(t *testing.T, d *Daemon, method, path string, body any) *httptest.ResponseRecorder {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	return w
}

func TestEnvironmentAPINeverReturnsValues(t *testing.T) {
	d := testDaemon(t)
	secret := "sk-very-secret-value"
	fileSecret := "file-secret-content"
	put := proto.EnvironmentUpdate{
		Vars:  map[string]*string{"TOKEN": &secret, "OTHER": &secret},
		Files: map[string]*proto.EnvironmentFile{"/home/user/.token": {Content: []byte(fileSecret)}},
	}
	for _, w := range []*httptest.ResponseRecorder{
		apiCall(t, d, "PUT", "/v1/environments/base", put),
		apiCall(t, d, "GET", "/v1/environments", nil),
	} {
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		// Content travels base64-encoded, so check that form too.
		encoded, _ := json.Marshal([]byte(fileSecret))
		for _, s := range []string{secret, fileSecret, strings.Trim(string(encoded), `"`)} {
			if strings.Contains(w.Body.String(), s) {
				t.Errorf("response contains a secret: %s", w.Body)
			}
		}
	}
	var list []proto.Environment
	json.Unmarshal(apiCall(t, d, "GET", "/v1/environments", nil).Body.Bytes(), &list)
	if len(list) != 1 || strings.Join(list[0].Vars, ",") != "OTHER,TOKEN" ||
		len(list[0].Files) != 1 || list[0].Files[0] != (proto.EnvironmentPath{Path: "/home/user/.token", Mode: 0o600}) {
		t.Errorf("list = %+v", list)
	}

	fi, err := os.Stat(d.environmentPath("base"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("environment file mode = %v, %v", fi.Mode(), err)
	}
}

func TestEnvironmentUpdateMerges(t *testing.T) {
	d := testDaemon(t)
	a, b := "a", "b"
	d.UpdateEnvironment("base", proto.EnvironmentUpdate{Vars: map[string]*string{"A": &a, "B": &b}})
	got, err := d.UpdateEnvironment("base", proto.EnvironmentUpdate{Vars: map[string]*string{"A": nil, "C": &a}})
	if err != nil || strings.Join(got.Vars, ",") != "B,C" {
		t.Errorf("after merge: %+v, %v", got, err)
	}
	if _, err := d.UpdateEnvironment("base", proto.EnvironmentUpdate{Vars: map[string]*string{"BAD-NAME": &a}}); err == nil {
		t.Error("invalid update succeeded")
	}
	if e, _ := d.environment("base"); e.Vars["B"] != "b" || len(e.Vars) != 2 {
		t.Errorf("a rejected update changed the stored environment: %v", e.Vars)
	}
}

func TestEnvironmentAPIRejects(t *testing.T) {
	d := testDaemon(t)
	for _, c := range []struct {
		method, path string
		body         string
		want         int
	}{
		{"PUT", "/v1/environments/none", `{}`, http.StatusBadRequest},
		{"PUT", "/v1/environments/base", `{"files":{"rel/path":{"content":""}}}`, http.StatusBadRequest},
		{"PUT", "/v1/environments/base", `{"files":{"/a":{"content":"","mode":"4755"}}}`, http.StatusBadRequest},
		{"PUT", "/v1/environments/base", `{"vars":` + strings.Repeat(" ", maxEnvUpdateBody) + `{}}`, http.StatusBadRequest},
		{"DELETE", "/v1/environments/missing", ``, http.StatusNotFound},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer t")
		w := httptest.NewRecorder()
		d.Handler().ServeHTTP(w, req)
		if w.Code != c.want {
			t.Errorf("%s %s %.40s: status %d, want %d: %s", c.method, c.path, c.body, w.Code, c.want, w.Body)
		}
	}
	req := httptest.NewRequest("GET", "/v1/environments", nil)
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("without a token: status %d", w.Code)
	}
}

func TestCreateEnvironment(t *testing.T) {
	d := testDaemon(t)
	if name, e, err := d.createEnvironment(""); name != "" || len(e.Vars) != 0 || err != nil {
		t.Errorf("without base: %q, %v, %v", name, e, err)
	}
	v := "v"
	d.UpdateEnvironment("base", proto.EnvironmentUpdate{Vars: map[string]*string{"A": &v}})
	if name, e, err := d.createEnvironment(""); name != "base" || e.Vars["A"] != "v" || err != nil {
		t.Errorf("default: %q, %v, %v", name, e, err)
	}
	if name, e, err := d.createEnvironment("none"); name != "" || len(e.Vars) != 0 || err != nil {
		t.Errorf("none: %q, %v, %v", name, e, err)
	}
	if _, _, err := d.createEnvironment("missing"); err == nil {
		t.Error("a missing environment succeeded")
	}
}
