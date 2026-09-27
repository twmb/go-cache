package cache

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file implements a small linearizability checker: each round runs a
// handful of concurrent operations against one or more keys, records every
// operation's invocation/response order and return values, and then
// verifies that SOME linearization exists for each key: a total order of
// the operations consistent with real time (op A precedes op B if A
// responded before B was invoked) under which the sequential model below
// produces exactly the observed returns. If no such order exists, an
// atomicity bug has been observed and the full history is dumped. Keys are
// independent, so checking each key's history alone is enough.
//
// The clock is frozen for each round (see testNow), so expiry is part of
// the sequential model.
//
// Get is modeled with its singleflight rules rather than as a plain map
// operation:
//
//   - A Get that runs its own load takes effect when the load finishes: the
//     key must not be visible then, and afterward holds the load's value.
//     Until then, the in-flight load looks absent to every other operation.
//   - If a Set or Swap replaces the in-flight load, the Get returns that
//     value, so the Get reads it at some point after the replacement.
//   - If a Delete or Clear removes the in-flight load, the Get still
//     returns its load's value, the key stays absent, and Gets that waited
//     on the load may also return that value (the model's ghost value).

type linKind uint8

const (
	linSet linKind = iota
	linSwap
	linDelete
	linTryGet
	linCAS
	linCAD
	linExpire
	linClean
	linGet
	linClear
)

func (k linKind) String() string {
	return [...]string{"Set", "Swap", "Delete", "TryGet", "CAS", "CAD", "Expire", "Clean", "Get", "Clear"}[k]
}

type linOp struct {
	kind linKind
	arg  int // value for Set/Swap, old for CAS/CAD, the load's value for Get
	arg2 int // new for CAS

	retV int
	retS KeyState
	retB bool

	// delOK, for a Get, is whether a Delete that returned Miss or a Clear
	// overlapped it, so its in-flight load may have been removed.
	delOK bool

	start, end int64 // global ticket counter values around the call
}

func (o *linOp) String() string {
	span := fmt.Sprintf("[%d,%d]", o.start, o.end)
	switch o.kind {
	case linSet:
		return fmt.Sprintf("Set(%d) %s", o.arg, span)
	case linSwap:
		return fmt.Sprintf("Swap(%d)=(%d,%v) %s", o.arg, o.retV, o.retS, span)
	case linDelete:
		return fmt.Sprintf("Delete=(%d,%v) %s", o.retV, o.retS, span)
	case linTryGet:
		return fmt.Sprintf("TryGet=(%d,%v) %s", o.retV, o.retS, span)
	case linCAS:
		return fmt.Sprintf("CAS(%d,%d)=%v %s", o.arg, o.arg2, o.retB, span)
	case linCAD:
		return fmt.Sprintf("CAD(%d)=%v %s", o.arg, o.retB, span)
	case linGet:
		return fmt.Sprintf("Get(load %d)=(%d,%v) delOK=%v %s", o.arg, o.retV, o.retS, o.delOK, span)
	default:
		return fmt.Sprintf("%v %s", o.kind, span)
	}
}

// linModel is the cache configuration and the frozen clock for a round.
type linModel struct {
	ttl, staleAge int64 // MaxAge and MaxStaleAge; 0 means unset
	now           int64
}

type linState struct {
	present bool
	val     int
	exp     int64 // 0 never expires
	ghost   int   // a removed in-flight Get's value that its waiters may return
}

func (m linModel) newExp() int64 {
	if m.ttl > 0 {
		return m.now + m.ttl
	}
	return 0
}

// class returns what TryGet reports for st.
func (m linModel) class(st linState) KeyState {
	switch {
	case !st.present:
		return Miss
	case st.exp == 0 || m.now < st.exp:
		return Hit
	case m.staleAge < 0 || m.staleAge > 0 && m.now < st.exp+m.staleAge:
		return Stale
	}
	return Miss
}

// linApply returns every state op can leave st in, given the op's recorded
// returns; none if the returns are impossible at this point in the order.
func (m linModel) linApply(st linState, o *linOp) []linState {
	readOK := func(s KeyState, v int) bool {
		c := m.class(st)
		return c == s && (c == Miss || v == st.val)
	}
	one := func(ok bool, next linState) []linState {
		if !ok {
			return nil
		}
		next.ghost = st.ghost
		return []linState{next}
	}
	live := m.class(st) == Hit
	switch o.kind {
	case linSet:
		return one(true, linState{present: true, val: o.arg, exp: m.newExp()})
	case linSwap:
		return one(readOK(o.retS, o.retV), linState{present: true, val: o.arg, exp: m.newExp()})
	case linDelete:
		return one(readOK(o.retS, o.retV), linState{})
	case linTryGet:
		return one(readOK(o.retS, o.retV), st)
	case linCAS:
		if live && st.val == o.arg {
			return one(o.retB, linState{present: true, val: o.arg2, exp: m.newExp()})
		}
		return one(!o.retB, st)
	case linCAD:
		if live && st.val == o.arg {
			return one(o.retB, linState{})
		}
		return one(!o.retB, st)
	case linExpire:
		if live {
			next := st
			next.exp = m.now - 1
			return one(true, next)
		}
		return one(true, st)
	case linClean:
		return one(true, st) // removes only what no read can see
	case linClear:
		return one(true, linState{})
	case linGet:
		switch {
		case o.retS == Hit:
			return one(live && st.val == o.retV || st.ghost != 0 && st.ghost == o.retV, st)
		case o.retS != Miss:
			return nil
		case o.retV != o.arg:
			// A Set or Swap replaced our load; we read its value.
			return one(live && st.val == o.retV, st)
		case live:
			return nil // we only load a key that is not visible
		}
		out := []linState{{present: true, val: o.arg, exp: m.newExp(), ghost: st.ghost}}
		if o.delOK {
			out = append(out, linState{ghost: o.arg})
		}
		return out
	}
	panic("unreachable")
}

// linearizable reports whether some real-time-consistent total order of ops
// explains every op's returns starting from initial.
func (m linModel) linearizable(initial linState, ops []*linOp) bool {
	used := make([]bool, len(ops))
	var rec func(st linState, depth int) bool
	rec = func(st linState, depth int) bool {
		if depth == len(ops) {
			return true
		}
	next:
		for i, o := range ops {
			if used[i] {
				continue
			}
			// o may go next only if no other unscheduled op wholly
			// preceded it in real time.
			for j, p := range ops {
				if !used[j] && j != i && p.end < o.start {
					continue next
				}
			}
			for _, st2 := range m.linApply(st, o) {
				used[i] = true
				if rec(st2, depth+1) {
					return true
				}
				used[i] = false
			}
		}
		return false
	}
	return rec(initial, 0)
}

// freezeClock sets now() to a fixed value for the rest of the test.
func freezeClock(t *testing.T) *atomic.Int64 {
	testNow.Store(int64(time.Hour))
	t.Cleanup(func() { testNow.Store(0) })
	return &testNow
}

type linConfig struct {
	opts   []Opt
	model  linModel
	kinds  []linKind
	keys   int                   // keys per round
	hashFn func(uintptr) uintptr // forces collisions if non-nil
	prime  func(c *Cache[int, int], k int, st *linState, m *linModel, clock *atomic.Int64)
}

func runLinearizability(t *testing.T, cfg linConfig, rounds int) {
	if testing.Short() {
		t.Skip("skipping stress test in -short")
	}
	const (
		opsPerKey = 4
		valSpace  = 3 // small domain so CAS/CAD old values plausibly match
	)
	clock := freezeClock(t)
	var c *Cache[int, int]
	newCache := func() {
		c = New[int, int](cfg.opts...)
		if cfg.hashFn != nil {
			c.t.hashFn = cfg.hashFn
		}
	}
	newCache()

	var ticket atomic.Int64
	for round := range rounds {
		if cfg.hashFn != nil && round%64 == 0 {
			newCache() // keep collision chains short
		}
		m := cfg.model
		type keyRound struct {
			k       int
			initial linState
			ops     []*linOp
		}
		krs := make([]*keyRound, cfg.keys)
		for i := range krs {
			kr := &keyRound{k: round*cfg.keys + i}
			if rand.IntN(2) == 1 {
				kr.initial = linState{present: true, val: 1 + rand.IntN(valSpace)}
				m.now = clock.Load()
				kr.initial.exp = m.newExp()
				c.Set(kr.k, kr.initial.val) // sequentially primed before the round
				if cfg.prime != nil {
					cfg.prime(c, kr.k, &kr.initial, &m, clock)
				}
			}
			for j := range opsPerKey {
				kr.ops = append(kr.ops, &linOp{
					kind: cfg.kinds[rand.IntN(len(cfg.kinds))],
					arg:  1 + rand.IntN(valSpace),
					arg2: 1 + rand.IntN(valSpace),
				})
				if o := kr.ops[j]; o.kind == linGet {
					o.arg = 100 + j // each Get's load returns a unique value
				}
			}
			krs[i] = kr
		}
		m.now = clock.Load()

		var wg sync.WaitGroup
		for _, kr := range krs {
			for _, o := range kr.ops {
				k := kr.k
				wg.Go(func() {
					o.start = ticket.Add(1)
					switch o.kind {
					case linSet:
						c.Set(k, o.arg)
					case linSwap:
						o.retV, _, o.retS = c.Swap(k, o.arg)
					case linDelete:
						o.retV, _, o.retS = c.Delete(k)
					case linTryGet:
						o.retV, _, o.retS = c.TryGet(k)
					case linCAS:
						o.retB = c.CompareAndSwap(k, o.arg, o.arg2)
					case linCAD:
						o.retB = c.CompareAndDelete(k, o.arg)
					case linExpire:
						c.Expire(k)
					case linClean:
						c.Clean()
					case linClear:
						c.Clear()
					case linGet:
						o.retV, _, o.retS = c.Get(k, func() (int, error) {
							for range rand.IntN(3) {
								runtime.Gosched()
							}
							return o.arg, nil
						})
					}
					o.end = ticket.Add(1)
				})
			}
		}
		wg.Wait()

		// A Clear on any key clears every key in this round.
		var clears []*linOp
		for _, kr := range krs {
			for _, o := range kr.ops {
				if o.kind == linClear {
					clears = append(clears, o)
				}
			}
		}
		for _, kr := range krs {
			ops := kr.ops
			for _, cl := range clears {
				if !contains(ops, cl) {
					ops = append(ops, cl)
				}
			}
			for _, o := range ops {
				if o.kind != linGet {
					continue
				}
				for _, p := range ops {
					overlap := p.start < o.end && o.start < p.end
					if overlap && (p.kind == linClear || p.kind == linDelete && p.retS == Miss) {
						o.delOK = true
					}
				}
			}
			if !m.linearizable(kr.initial, ops) {
				var dump strings.Builder
				for _, o := range ops {
					dump.WriteString("\n\t")
					dump.WriteString(o.String())
				}
				t.Fatalf("round %d key %d: no linearization explains this history (initial %+v, now %d):%s",
					round, kr.k, kr.initial, m.now, dump.String())
			}
		}
	}
}

func contains(ops []*linOp, o *linOp) bool {
	for _, p := range ops {
		if p == o {
			return true
		}
	}
	return false
}

var linMapKinds = []linKind{linSet, linSwap, linDelete, linTryGet, linCAS, linCAD, linExpire, linClean, linGet, linClear}

// TestLinearizability checks every map operation, Get, Expire, Clean, and
// Clear against the model with no expiry configured.
func TestLinearizability(t *testing.T) {
	runLinearizability(t, linConfig{kinds: linMapKinds, keys: 1}, 30_000)
}

// TestLinearizabilityCollisions is TestLinearizability with several keys
// per round whose hashes collide, so overflow chains and trie expansion and
// pruning race with each other.
func TestLinearizabilityCollisions(t *testing.T) {
	runLinearizability(t, linConfig{
		kinds:  linMapKinds,
		keys:   3,
		hashFn: func(h uintptr) uintptr { return h & 3 }, // 4 full hashes, 15 shared nibbles
	}, 20_000)
}

// TestLinearizabilityExpiry checks the map operations, Expire, and Clean
// with MaxAge and MaxStaleAge. Before each round, a primed key is aged into
// its live, stale, or dead phase by moving the frozen clock.
func TestLinearizabilityExpiry(t *testing.T) {
	const ttl, staleAge = 100, 50
	for _, sa := range []int64{0, staleAge, -1} {
		t.Run(fmt.Sprint("stale_age_", sa), func(t *testing.T) {
			runLinearizability(t, linConfig{
				opts:  []Opt{MaxAge(ttl), MaxStaleAge(time.Duration(sa))},
				model: linModel{ttl: ttl, staleAge: sa},
				kinds: []linKind{linSet, linSwap, linDelete, linTryGet, linCAS, linCAD, linExpire, linClean},
				keys:  1,
				prime: func(c *Cache[int, int], k int, st *linState, m *linModel, clock *atomic.Int64) {
					if rand.IntN(4) == 0 {
						m.now = clock.Load()
						c.Expire(k)
						st.exp = m.now - 1
					}
					// Age into live, the expiry instant, stale, the window
					// close, or dead.
					ages := []int64{0, ttl - 1, ttl, ttl + staleAge/2, ttl + staleAge, ttl + 2*staleAge}
					clock.Add(ages[rand.IntN(len(ages))])
				},
			}, 20_000)
		})
	}
}
