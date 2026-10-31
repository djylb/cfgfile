package cfgfile

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/djylb/cfgfile/jwcc"
)

// ErrRemoved is reported by a Watcher when the file disappears; the
// previous value stays in effect until the file is back.
var ErrRemoved = errors.New("cfgfile: configuration file removed")

// WatchOptions configures Watch.
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
}

// Change is a configuration delivered by a Watcher.
type Change[T any] struct {
	// Result is the new configuration as Load returned it.
	Result *Result[T]
	// Changed lists the top-level members whose value differs from the
	// previous configuration, as the struct encodes them. Which of them can
	// be applied without a restart is the caller's decision.
	Changed []jwcc.Path
}

// Watcher reloads a configuration file when it changes. See Watch.
type Watcher[T any] struct {
	path string
	o    *Options
	wo   WatchOptions[T]

	reload chan struct{}
	cancel context.CancelFunc
	done   chan struct{}

	// op serialises reading the file with Save and Update.
	op       sync.Mutex
	baseHash [sha256.Size]byte
	baseOK   bool // baseHash is valid (the file existed)
	stat     fs.FileInfo
	removed  bool

	mu     sync.Mutex // guards cur and closed
	cur    *Result[T]
	closed bool
}

// Watch starts watching path, starting from cur, the result of a Load of
// the same path with the same Options. It polls the file every Interval:
// a change in size, modification time or file identity (an atomic rename
// replaces it), or a recent modification time (coarse clocks), leads to a
// content hash; only different content is loaded. A reload runs the whole
// Load pipeline: defaults, migrations (written back only with
// Options.Persist), overlay and validation. Invalid results and removals
// go to OnError and keep the previous value; valid results that differ from
// the previous value go to OnChange.
//
// Watch starts exactly one goroutine, which ends when ctx is done or Close
// is called; Done is closed when it has ended. Callbacks run on that
// goroutine, one at a time, never concurrently with each other.
func Watch[T any](ctx context.Context, path string, o *Options, wo WatchOptions[T], cur *Result[T]) (*Watcher[T], error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	if cur == nil || path == "" {
		return nil, errors.New("cfgfile: Watch needs a path and the result of loading it")
	}
	if wo.Interval <= 0 {
		wo.Interval = 2 * time.Second
	}
	if wo.Debounce <= 0 {
		wo.Debounce = 200 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(ctx)
	w := &Watcher[T]{path: path, o: o, wo: wo, reload: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{}), cur: cur}
	w.setBase(cur.Snapshot)
	w.stat, _ = os.Stat(path)
	go w.run(ctx)
	return w, nil
}

func (w *Watcher[T]) setBase(s Snapshot) { w.baseHash, w.baseOK = s.Hash, s.Exists }

// Current returns the configuration in effect. Safe for concurrent use.
func (w *Watcher[T]) Current() *Result[T] {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cur
}

// Reload asks for a reload now, for example on SIGHUP, even if the file
// looks unchanged; the environment is read again too. It does not block;
// requests made while one is pending are merged.
func (w *Watcher[T]) Reload() {
	select {
	case w.reload <- struct{}{}:
	default:
	}
}

// Close stops the watcher. It is idempotent and does not block. The
// goroutine commits to each callback under the watcher's lock: after Close
// returns no callback is committed, so at most the one committed before
// Close can still be running (or about to run); Done is closed after it
// returns. A callback may call Close, Save and Update.
func (w *Watcher[T]) Close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.cancel()
}

// Done is closed when the watcher's goroutine has ended.
func (w *Watcher[T]) Done() <-chan struct{} { return w.done }

// Save is Save on the watched file using the current snapshot; the write
// does not trigger a reload, and Current is refreshed without a callback.
// If the file was written but reloading it fails (for example the
// environment no longer validates), Current keeps the previous value with
// the new snapshot and the error is returned.
func (w *Watcher[T]) Save(v *T) error {
	return w.write(func(s Snapshot) (Snapshot, error) { return Save(w.path, s, v, w.o) })
}

// Update is Update on the watched file, as Save.
func (w *Watcher[T]) Update(fn func(doc *jwcc.Value) error) error {
	return w.write(func(s Snapshot) (Snapshot, error) { return Update[T](w.path, s, w.o, fn) })
}

func (w *Watcher[T]) write(f func(Snapshot) (Snapshot, error)) error {
	w.op.Lock()
	defer w.op.Unlock()
	snap, werr := f(w.Current().Snapshot)
	if werr != nil && !errors.Is(werr, ErrNotDurable) {
		return werr
	}
	w.setBase(snap)
	w.stat, _ = os.Stat(w.path)
	w.removed = false
	o := *w.o
	o.Persist = false
	data, err := readFile(w.path, &o)
	var r *Result[T]
	if err == nil {
		r, err = loadData[T](w.path, data, true, &o)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		cur := *w.cur
		cur.Snapshot = snap
		w.cur = &cur
		return fmt.Errorf("cfgfile: file written, but reloading it failed: %w", err)
	}
	overrides := r.Snapshot.Overrides
	r.Snapshot = snap
	r.Snapshot.Overrides = overrides
	w.cur = r
	return werr
}

func (w *Watcher[T]) run(ctx context.Context) {
	defer close(w.done)
	tick := time.NewTicker(w.wo.Interval)
	defer tick.Stop()
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	defer debounce.Stop()
	pending, rounds := false, 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.reload:
			pending = false
			debounce.Stop()
			w.check(true)
		case <-tick.C:
			if moved, recent := w.changed(); !pending && (moved || recent) {
				pending, rounds = true, 0
				debounce.Reset(w.wo.Debounce)
			}
		case <-debounce.C:
			// Wait until the file holds still, but not forever.
			if moved, _ := w.changed(); moved && rounds < 10 {
				rounds++
				debounce.Reset(w.wo.Debounce)
				continue
			}
			pending = false
			w.check(false)
		}
	}
}

// changed stats the file and reports whether it differs from the last
// stat, and whether its modification time is so recent that a coarse
// clock could hide a further write (then only the hash can tell).
func (w *Watcher[T]) changed() (moved, recent bool) {
	w.op.Lock()
	defer w.op.Unlock()
	st, err := os.Stat(w.path)
	if err != nil {
		moved = w.stat != nil || !w.removed
		w.stat = nil
		return moved, false
	}
	prev := w.stat
	w.stat = st
	moved = prev == nil || !os.SameFile(prev, st) || prev.Size() != st.Size() || !prev.ModTime().Equal(st.ModTime())
	return moved, time.Since(st.ModTime()) < 2*time.Second+w.wo.Interval
}

// check reloads the file if its content differs from the baseline (or
// always, if forced) and runs the callback.
func (w *Watcher[T]) check(force bool) {
	w.op.Lock()
	data, err := readFile(w.path, w.o)
	var cb func()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !w.removed {
			w.removed = true
			w.baseOK = false
			cb = w.errorCallback(&Error{File: w.path, Err: ErrRemoved})
		}
	case err != nil:
		cb = w.errorCallback(err)
	case !force && w.baseOK && sha256.Sum256(data) == w.baseHash:
		// Same content: a touch, or our own write.
	default:
		w.removed = false
		w.baseHash, w.baseOK = sha256.Sum256(data), true
		r, err := loadData[T](w.path, data, true, w.o)
		if err != nil {
			cb = w.errorCallback(err)
			break
		}
		if r.Persisted {
			w.setBase(r.Snapshot)
			w.stat, _ = os.Stat(w.path)
		}
		cb = w.changeCallback(r)
	}
	w.op.Unlock()
	if cb != nil {
		cb()
	}
}

func (w *Watcher[T]) errorCallback(err error) func() {
	if w.wo.OnError == nil {
		return nil
	}
	return func() {
		if w.begin() {
			w.wo.OnError(err)
		}
	}
}

func (w *Watcher[T]) changeCallback(r *Result[T]) func() {
	w.mu.Lock()
	prev := w.cur
	changed := topLevelDiff(prev.Value, r.Value)
	w.cur = r
	w.mu.Unlock()
	if len(changed) == 0 || w.wo.OnChange == nil {
		return nil
	}
	return func() {
		if w.begin() {
			w.wo.OnChange(Change[T]{Result: r, Changed: changed})
		}
	}
}

// begin reports whether a callback may start: the watcher is not closed.
func (w *Watcher[T]) begin() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.closed
}

func topLevelDiff[T any](a, b T) []jwcc.Path {
	if reflect.DeepEqual(a, b) {
		return nil
	}
	ta, err1 := toTree(reflect.ValueOf(&a).Elem())
	tb, err2 := toTree(reflect.ValueOf(&b).Elem())
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
