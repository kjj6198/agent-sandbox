package proto

import "testing"

func TestEgressAllows(t *testing.T) {
	e, err := NewEgress([]string{"GitHub.com.", "*.npmjs.org"})
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		"github.com":          true,
		"GITHUB.COM.":         true,
		"api.github.com":      false,
		"registry.npmjs.org":  true,
		"a.b.npmjs.org":       true,
		"npmjs.org":           false,
		"evilnpmjs.org":       false,
		"github.com.evil.com": false,
	} {
		if got := e.Allows(host); got != want {
			t.Errorf("Allows(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestNewEgressRejects(t *testing.T) {
	for _, h := range []string{"", "*", "*.com.", "https://github.com", "github.com:443", "a.*.com", "localhost"} {
		if _, err := NewEgress([]string{h}); err == nil {
			t.Errorf("NewEgress(%q) accepted", h)
		}
	}
}
