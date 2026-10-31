package cfgfile_test

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djylb/cfgfile"
	"github.com/djylb/cfgfile/jwcc"
)

type watchEnv struct {
	t       *testing.T
	path    string
	f       *cfgfile.File[Config]
	w       *cfgfile.Watcher
	changes chan cfgfile.Change[Config]
	errs    chan error
}

func startWatch(t *testing.T, o *cfgfile.Options) *watchEnv {
	t.Helper()
	e := &watchEnv{t: t, path: write(t, v2), changes: make(chan cfgfile.Change[Config], 16), errs: make(chan error, 16)}
	var err error
	e.f, err = cfgfile.Open[Config](e.path, o)
	if err != nil {
		t.Fatal(err)
	}
	e.w, err = e.f.Watch(context.Background(), cfgfile.WatchOptions[Config]{
		Interval: 10 * time.Millisecond,
		Debounce: 20 * time.Millisecond,
		OnChange: func(c cfgfile.Change[Config]) { e.changes <- c },
		OnError:  func(err error) { e.errs <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.w.Close()
		<-e.w.Done()
	})
	return e
}

func (e *watchEnv) change() cfgfile.Change[Config] {
	e.t.Helper()
	var c cfgfile.Change[Config]
	select {
	case c = <-e.changes:
	case err := <-e.errs:
		e.t.Fatalf("unexpected error: %v", err)
	case <-time.After(5 * time.Second):
		e.t.Fatal("no change delivered")
	}
	return c
}

func (e *watchEnv) err() error {
	e.t.Helper()
	select {
	case err := <-e.errs:
		return err
	case c := <-e.changes:
		e.t.Fatalf("unexpected change: %v", c.Changed)
	case <-time.After(5 * time.Second):
		e.t.Fatal("no error delivered")
	}
	return nil
}

func (e *watchEnv) quiet() {
	e.t.Helper()
	select {
	case c := <-e.changes:
		e.t.Fatalf("unexpected change: %v", c.Changed)
	case err := <-e.errs:
		e.t.Fatalf("unexpected error: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
}

func (e *watchEnv) put(s string) {
	if err := os.WriteFile(e.path, []byte(s), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func TestWatch(t *testing.T) {
	e := startWatch(t, opts())

	// In-place edit.
	e.put(strings.Replace(v2, `"debug"`, `"warn"`, 1))
	c := e.change()
	if c.New.Log.Level != "warn" || len(c.Changed) != 1 || c.Changed[0].String() != "log" {
		t.Fatalf("%v %+v", c.Changed, c.New.Log)
	}

	// Comment-only edit: no callback.
	e.put(strings.Replace(v2, `"debug"`, `"warn"`, 1) + "// tail\n")
	e.quiet()

	// Atomic replace by rename, same size as before.
	tmp := e.path + ".new"
	mustWrite(t, tmp, []byte(strings.Replace(v2, `"debug"`, `"error"`, 1)+"// tail\n"), 0o644)
	if err := os.Rename(tmp, e.path); err != nil {
		t.Fatal(err)
	}
	if c := e.change(); c.New.Log.Level != "error" {
		t.Fatal(c.New.Log)
	}

	// Invalid content keeps the previous value and reports a position.
	e.put("{\"version\": 2,\n  \"log\": {\"levle\": \"x\"}}")
	if err := e.err(); !strings.Contains(err.Error(), ":2:11: log.levle: unknown member") {
		t.Fatal(err)
	}
	if e.f.Value().Log.Level != "error" {
		t.Fatal("previous value lost")
	}

	// Removal, then re-creation.
	must(t, os.Remove(e.path))
	if err := e.err(); !errors.Is(err, cfgfile.ErrRemoved) {
		t.Fatal(err)
	}
	e.put(v2)
	if c := e.change(); c.New.Log.Level != "debug" {
		t.Fatal(c.New.Log)
	}

	// Replace by delete and create (as some editors and platforms do)
	// within the debounce window: one change, no removal.
	must(t, os.Remove(e.path))
	e.put(strings.Replace(v2, `"debug"`, `"info"`, 1))
	if c := e.change(); c.New.Log.Level != "info" {
		t.Fatal(c.New.Log)
	}
}

func TestWatchOwnWrites(t *testing.T) {
	e := startWatch(t, opts())
	c := e.f.Value()
	c.DataDir = "/srv"
	if err := e.f.Save(&c); err != nil {
		t.Fatal(err)
	}
	if err := e.f.Update(func(doc *jwcc.Value) error { return doc.SetValue(jwcc.Path{"log", "format"}, "json") }); err != nil {
		t.Fatal(err)
	}
	e.quiet()
	if cur := e.f.Value(); cur.DataDir != "/srv" || cur.Log.Format != "json" {
		t.Fatalf("%+v", cur)
	}
	// A later outside edit is still seen.
	got, _ := os.ReadFile(e.path)
	e.put(strings.Replace(string(got), `"/srv"`, `"/opt"`, 1))
	if c := e.change(); c.New.DataDir != "/opt" || c.Changed[0].String() != "data_dir" {
		t.Fatal(c.Changed)
	}
}

func TestWatchReload(t *testing.T) {
	o := opts()
	o.EnvPrefix = "APP_"
	var level atomic.Value
	level.Store("warn")
	o.LookupEnv = func(k string) (string, bool) {
		if k == "APP_LOG_LEVEL" {
			return level.Load().(string), true
		}
		return "", false
	}
	e := startWatch(t, o)
	e.w.Reload()
	e.quiet() // nothing changed
	level.Store("error")
	e.w.Reload()
	if c := e.change(); c.New.Log.Level != "error" {
		t.Fatal(c.New.Log)
	}
}

func TestWatchLifetime(t *testing.T) {
	base := runtime.NumGoroutine()
	p := write(t, v2)
	o := opts()
	f, err := cfgfile.Open[Config](p, o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := make(chan struct{}, 100)
	w, err := f.Watch(ctx, cfgfile.WatchOptions[Config]{
		Interval: time.Millisecond, Debounce: time.Millisecond,
		OnChange: func(cfgfile.Change[Config]) { calls <- struct{}{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-w.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("not stopped by ctx")
	}
	w.Close()
	w.Close()
	mustWrite(t, p, []byte(strings.Replace(v2, `"debug"`, `"warn"`, 1)), 0o644)
	w.Reload()
	time.Sleep(20 * time.Millisecond)
	if len(calls) != 0 {
		t.Fatal("callback after stop")
	}
	// Close from inside a callback; Close never blocks.
	// A stopped watch can be replaced; two at once cannot.
	w3, err := f.Watch(context.Background(), cfgfile.WatchOptions[Config]{})
	if err != nil {
		t.Fatal("cannot watch again after stop:", err)
	}
	if _, err := f.Watch(context.Background(), cfgfile.WatchOptions[Config]{}); err == nil {
		t.Fatal("second watcher accepted")
	}
	w3.Close()
	<-w3.Done()
	var w2 *cfgfile.Watcher
	started := make(chan struct{})
	f2, err := cfgfile.Open[Config](p, o)
	if err != nil {
		t.Fatal(err)
	}
	w2, err = f2.Watch(context.Background(), cfgfile.WatchOptions[Config]{
		Interval: time.Millisecond, Debounce: time.Millisecond,
		OnChange: func(cfgfile.Change[Config]) { <-started; w2.Close() },
	})
	if err != nil {
		t.Fatal(err)
	}
	close(started)
	mustWrite(t, p, []byte(v2), 0o644)
	select {
	case <-w2.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("not closed from callback")
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Fatalf("goroutines: %d > %d", n, base)
	}
}
