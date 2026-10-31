package cfgfile

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/djylb/cfgfile/jwcc"
	"github.com/djylb/timeshim"
)

// WatchOptions configures File.Watch.
type WatchOptions[T any] struct {
	// Interval between checks of the file; default 2s.
	Interval time.Duration
	// Debounce is how long the file must stay unchanged after a change is
	// seen before it is read; default 200ms. A burst of writes is read once.
	Debounce time.Duration
	// OnChange receives every new valid configuration that differs from
	// the previous one.
	OnChange func(Change[T])
	// OnError receives failed reloads (with file positions where known)
	// and ErrRemoved. The previous value stays in effect.
	OnError func(error)
	// Clock schedules the polling; nil uses timeshim.Real. Tests can supply
	// a timeshim.Fake to run the watcher deterministically. The check for a
	// recently modified file compares file times with real time whatever
	// the clock, since file times come from the real file system.
	Clock timeshim.Clock
}

// Change describes a reload that changed the value in effect.
type Change[T any] struct {
	Old, New T
	// Changed lists the top-level members whose value differs, as the
	// struct encodes them. Which of them can be applied without a restart
	// is the caller's decision.
	Changed []jwcc.Path
	// Result is the new load result (warnings, migration).
	Result *Result[T]
}

// Watcher is a running File.Watch.
type Watcher struct {
	reload chan struct{}
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	closed bool
}

// Watch reloads the file when it changes and reports to wo's callbacks.
// It polls every Interval: a change in size, modification time or file
// identity (an atomic rename replaces it), or a recent modification time
// (coarse clocks), leads to a content hash; only content that differs from
// the value in effect is loaded, after Debounce of quiet. A reload runs the
// whole Load pipeline (migrations are written back only with
// Options.Persist). Invalid results and removal go to OnError and keep the
// previous value; a valid value that differs from the previous one becomes
// the value in effect and goes to OnChange. The file's own Save and Update
// do not cause callbacks.
//
// Watch starts exactly one goroutine, which ends when ctx is done or
// Watcher.Close is called. Callbacks run on it one at a time. A File has at
// most one Watcher at a time.
func (f *File[T]) Watch(ctx context.Context, wo WatchOptions[T]) (*Watcher, error) {
	f.mu.Lock()
	if f.watching {
		f.mu.Unlock()
		return nil, errors.New("cfgfile: file is already watched")
	}
	f.watching = true
	f.mu.Unlock()
	if wo.Interval <= 0 {
		wo.Interval = 2 * time.Second
	}
	if wo.Debounce <= 0 {
		wo.Debounce = 200 * time.Millisecond
	}
	if wo.Clock == nil {
		wo.Clock = timeshim.Real
	}
	ctx, cancel := context.WithCancel(ctx)
	w := &Watcher{reload: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{})}
	p := &poller[T]{f: f, w: w, wo: wo}
	p.stat, _ = os.Stat(f.path)
	go p.run(ctx)
	return w, nil
}

// Reload asks for a reload now, for example on SIGHUP, even if the file
// looks unchanged; the environment is read again too. It does not block;
// requests made while one is pending are merged.
func (w *Watcher) Reload() {
	select {
	case w.reload <- struct{}{}:
	default:
	}
}

// Close stops the watcher. It is idempotent and does not block. The
// goroutine commits to each callback under the watcher's lock: after Close
// returns no callback is committed, so at most the one committed before
// Close can still be running (or about to run); Done is closed after it
// returns. A callback may call Close and the File's methods.
func (w *Watcher) Close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.cancel()
}

// Done is closed when the watcher's goroutine has ended.
func (w *Watcher) Done() <-chan struct{} { return w.done }

// begin reports whether a callback may start: the watcher is not closed.
func (w *Watcher) begin() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.closed
}

type poller[T any] struct {
	f    *File[T]
	w    *Watcher
	wo   WatchOptions[T]
	stat fs.FileInfo // owned by the goroutine
}

func (p *poller[T]) run(ctx context.Context) {
	defer close(p.w.done)
	defer func() {
		p.f.mu.Lock()
		p.f.watching = false
		p.f.mu.Unlock()
	}()
	// The ticker polls at a fixed rate; all work happens on this goroutine.
	tick := timeshim.NewTicker(p.wo.Clock, p.wo.Interval)
	defer tick.Stop()
	debounce := timeshim.NewTimer(p.wo.Clock, time.Hour)
	debounce.Stop()
	defer debounce.Stop()
	pending, rounds := false, 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.w.reload:
			pending = false
			debounce.Stop() // also drops a fired, unreceived debounce
			p.check(true)
		case <-tick.C:
			if moved, recent := p.changed(); !pending && (moved || recent) {
				pending, rounds = true, 0
				debounce.Reset(p.wo.Debounce)
			}
		case <-debounce.C:
			// Wait until the file holds still, but not forever.
			if moved, _ := p.changed(); moved && rounds < 10 {
				rounds++
				debounce.Reset(p.wo.Debounce)
				continue
			}
			pending = false
			p.check(false)
		}
	}
}

// changed stats the file and reports whether it differs from the last
// stat, and whether its modification time is so recent that a coarse
// clock could hide a further write (then only the hash can tell).
func (p *poller[T]) changed() (moved, recent bool) {
	st, err := os.Stat(p.f.path)
	if err != nil {
		moved = p.stat != nil
		p.stat = nil
		return moved, false
	}
	prev := p.stat
	p.stat = st
	moved = prev == nil || !os.SameFile(prev, st) || prev.Size() != st.Size() || !prev.ModTime().Equal(st.ModTime())
	return moved, time.Since(st.ModTime()) < 2*time.Second+p.wo.Interval // file times are real
}

func (p *poller[T]) check(force bool) {
	old, now, err := p.f.reload(force)
	switch {
	case err != nil:
		if p.wo.OnError != nil && p.w.begin() {
			p.wo.OnError(err)
		}
	case now != nil:
		changed := topLevelDiff(old.Value, now.Value)
		if len(changed) > 0 && p.wo.OnChange != nil && p.w.begin() {
			p.wo.OnChange(Change[T]{Old: old.Value, New: now.Value, Changed: changed, Result: now})
		}
	}
}

func topLevelDiff[T any](a, b T) []jwcc.Path {
	if reflect.DeepEqual(a, b) {
		return nil
	}
	ta, err1 := toTree(reflect.ValueOf(&a).Elem(), false)
	tb, err2 := toTree(reflect.ValueOf(&b).Elem(), false)
	am, _ := ta.(map[string]any)
	bm, _ := tb.(map[string]any)
	if err1 != nil || err2 != nil || am == nil || bm == nil {
		return []jwcc.Path{nil}
	}
	keys := sortedKeys(am)
	for _, k := range sortedKeys(bm) {
		if _, ok := am[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var out []jwcc.Path
	for _, k := range keys {
		av, aok := am[k]
		bv, bok := bm[k]
		if aok != bok || !reflect.DeepEqual(av, bv) {
			out = append(out, jwcc.Path{k})
		}
	}
	if out == nil {
		out = []jwcc.Path{nil} // differs only in unexported or unencoded state
	}
	return out
}
