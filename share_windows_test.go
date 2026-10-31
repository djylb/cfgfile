//go:build windows

package cfgfile

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReplaceWhileOpen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "app.jsonc")
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Our own reader shares delete: replacing succeeds, and the reader
	// still sees the old content.
	r, err := openShared(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(p, []byte(`{"a": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := io.ReadAll(r)
	if err != nil || string(old) != "{}" {
		t.Fatal(string(old), err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != `{"a": 1}` {
		t.Fatalf("%q", got)
	}

	// Another program's default open blocks replacement: bounded retries,
	// then a clear error, and the temporary file is removed.
	h, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = WriteFile(p, []byte(`{"a": 2}`), 0o600)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "another program holds it open") {
		t.Fatal(err)
	}
	if elapsed < replaceRetry/2 || elapsed > 10*replaceRetry {
		t.Fatalf("retried for %v", elapsed)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".app.jsonc.tmp-*")); len(left) != 0 {
		t.Fatal("temporary file left:", left)
	}
	// Once it is closed, writing works again.
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(p, []byte(`{"a": 3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Released during the retries: the write succeeds.
	h, err = os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = h.Close() // the test only needs the handle gone
		close(done)
	}()
	if err := WriteFile(p, []byte(`{"a": 4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	<-done
}
