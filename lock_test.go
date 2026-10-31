package cfgfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockWrites(t *testing.T) {
	if !lockSupported {
		t.Skip("no advisory locks on this platform")
	}
	p := filepath.Join(t.TempDir(), "c.jsonc")
	o := &Options{LockWrites: true}
	unlock, err := lockWrites(p, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p + ".lock"); err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{})
	go func() {
		u, err := lockWrites(p, o)
		if err != nil {
			t.Error(err)
		} else {
			u()
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("second writer did not wait")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("second writer never got the lock")
	}
	// Without the option nothing is created.
	q := filepath.Join(t.TempDir(), "d.jsonc")
	u, _ := lockWrites(q, &Options{})
	u()
	if _, err := os.Stat(q + ".lock"); err == nil {
		t.Fatal("lock file without LockWrites")
	}
}
