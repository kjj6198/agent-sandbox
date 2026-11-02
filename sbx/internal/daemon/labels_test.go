//go:build linux

package daemon

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

func TestMatchLabels(t *testing.T) {
	labels := map[string]string{"app": "hinacode", "thread": "t1", "empty": ""}
	for _, c := range []struct {
		params []string
		want   bool
	}{
		{nil, true},
		{[]string{"app=hinacode"}, true},
		{[]string{"app=hinacode", "thread=t1"}, true},
		{[]string{"app=hinacode", "thread=t2"}, false},
		{[]string{"app=other"}, false},
		{[]string{"missing="}, false},
		{[]string{"empty="}, true},
		{[]string{"thread=t1=x"}, false},
	} {
		sel, err := parseLabelSelector(c.params)
		if err != nil {
			t.Fatalf("%v: %v", c.params, err)
		}
		if got := matchLabels(labels, sel); got != c.want {
			t.Errorf("%v: match = %v, want %v", c.params, got, c.want)
		}
	}
	if matchLabels(nil, map[string]string{"app": ""}) {
		t.Error("a sandbox without labels matched a filter")
	}
	for _, bad := range []string{"app", "=x", "bad key=x"} {
		if _, err := parseLabelSelector([]string{bad}); err == nil {
			t.Errorf("parseLabelSelector(%q) succeeded", bad)
		}
	}
}

func TestValidLabels(t *testing.T) {
	if err := validLabels(map[string]string{"hinacode.dev/thread": "t1", "a": ""}); err != nil {
		t.Errorf("validLabels: %v", err)
	}
	many := map[string]string{}
	for i := range maxLabels + 1 {
		many[strings.Repeat("k", i+1)] = ""
	}
	for name, labels := range map[string]map[string]string{
		"too many":    many,
		"empty key":   {"": "v"},
		"space":       {"a b": "v"},
		"long value":  {"a": strings.Repeat("v", maxLabelValue+1)},
		"leading dot": {".a": "v"},
	} {
		if validLabels(labels) == nil {
			t.Errorf("%s: validLabels succeeded", name)
		}
	}
}

func TestListFiltersByLabel(t *testing.T) {
	d := testDaemon(t)
	for id, labels := range map[string]map[string]string{
		"sb-a": {"app": "hinacode", "thread": "t1"},
		"sb-b": {"app": "hinacode", "thread": "t2"},
		"sb-c": nil,
	} {
		d.sandboxes[id] = &sandbox{state: sandboxState{ID: id, Labels: labels}, vm: &vm{}}
	}
	ids := func(query string) string {
		w := apiCall(t, d, "GET", "/v1/sandboxes"+query, nil)
		if w.Code != http.StatusOK {
			return w.Body.String()
		}
		var list []proto.Sandbox
		json.Unmarshal(w.Body.Bytes(), &list)
		var out []string
		for _, sb := range list {
			out = append(out, sb.ID)
		}
		slices.Sort(out)
		return strings.Join(out, ",")
	}
	for query, want := range map[string]string{
		"":                                    "sb-a,sb-b,sb-c",
		"?label=app=hinacode":                 "sb-a,sb-b",
		"?label=app=hinacode&label=thread=t2": "sb-b",
		"?label=app=nobody":                   "",
	} {
		if got := ids(query); got != want {
			t.Errorf("GET /v1/sandboxes%s = %q, want %q", query, got, want)
		}
	}
	if w := apiCall(t, d, "GET", "/v1/sandboxes?label=app", nil); w.Code != http.StatusBadRequest {
		t.Errorf("malformed filter: status %d", w.Code)
	}
}
