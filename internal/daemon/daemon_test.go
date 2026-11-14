//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "token")
	first, err := loadOrCreateToken(path)
	if err != nil || len(first) < 20 {
		t.Fatalf("create: %q, %v", first, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode: %v, %v", info.Mode(), err)
	}
	again, err := loadOrCreateToken(path)
	if err != nil || again != first {
		t.Fatalf("second start read %q, %v; want %q", again, err, first)
	}
}
