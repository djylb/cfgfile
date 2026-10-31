package cfgfile_test

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/djylb/cfgfile"
	"github.com/djylb/timeshim"
)

func TestWatchFakeClock(t *testing.T) {
	base := runtime.NumGoroutine()
	p := write(t, v2)
	f, err := cfgfile.Open[Config](p, opts())
	if err != nil {
		t.Fatal(err)
	}
	clk := timeshim.NewFake(time.Now())
	changes := make(chan cfgfile.Change[Config], 4)
	w, err := f.Watch(context.Background(), cfgfile.WatchOptions[Config]{
		Interval: time.Second, Debounce: 100 * time.Millisecond, Clock: clk,
		OnChange: func(c cfgfile.Change[Config]) { changes <- c },
		OnError:  func(err error) { t.Error(err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	// next drives polls until a change is delivered. The watcher handles
	// each tick and debounce on its own goroutine, so each round gives it a
	// moment of real time; the round count bounds the wait.
	next := func() cfgfile.Change[Config] {
		for range 2000 {
			select {
			case c := <-changes:
				return c
			case <-time.After(time.Millisecond):
			}
			clk.Advance(100 * time.Millisecond)
		}
		select {
		case c := <-changes:
			return c
		case <-time.After(10 * time.Second): // a safety net, not a wait
			t.Fatal("no change")
		}
		return cfgfile.Change[Config]{}
	}
	// Writes are atomic: the watcher may poll at any moment and must not
	// see a half-written file.
	must(t, cfgfile.WriteFile(p, []byte(strings.Replace(v2, `"debug"`, `"warn"`, 1)), 0o644))
	if c := next(); c.New.Log.Level != "warn" || c.Old.Log.Level != "debug" {
		t.Fatalf("%+v", c)
	}
	// An own write causes no callback: the next one is the outside edit.
	c := f.Value()
	c.DataDir = "/own"
	must(t, f.Save(&c))
	got, err := readText(p)
	if err != nil {
		t.Fatal(err)
	}
	must(t, cfgfile.WriteFile(p, []byte(strings.Replace(got, `"warn"`, `"error"`, 1)), 0o644))
	if c := next(); c.New.Log.Level != "error" || c.Old.DataDir != "/own" {
		t.Fatalf("%+v", c)
	}
	w.Close()
	<-w.Done()
	for runtime.NumGoroutine() > base {
		runtime.Gosched()
	}
}

func readText(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}
