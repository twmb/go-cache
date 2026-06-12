package cache

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// This file implements a small linearizability checker: each round runs a
// handful of concurrent map operations against a single key, records every
// operation's invocation/response order and return values, and then
// verifies that SOME linearization exists — a total order of the operations
// consistent with real time (op A precedes op B if A responded before B was
// invoked) under which sequential map semantics produce exactly the
// observed returns. If no such order exists, an atomicity bug has been
// observed and the full history is dumped.
//
// Get is deliberately excluded from histories: its singleflight contract
// makes it non-atomic by design (a concurrent Swap cancels an in-flight
// load and the Get returns the Swap's value with a Miss state), which is
// documented behavior but inexpressible as a single sequential operation.
// The cache is configured with no expiry options, so the model is exactly
// a map.

type linKind uint8

const (
	linSet linKind = iota
	linSwap
	linDelete
	linTryGet
	linCAS
	linCAD
)

func (k linKind) String() string {
	return [...]string{"Set", "Swap", "Delete", "TryGet", "CAS", "CAD"}[k]
}

type linOp struct {
	kind linKind
	arg  int // value for Set/Swap, old for CAS/CAD
	arg2 int // new for CAS

	retV int
	retS KeyState
	retB bool

	start, end int64 // global ticket counter values around the call
}

func (o *linOp) String() string {
	switch o.kind {
	case linSet:
		return fmt.Sprintf("Set(%d) [%d,%d]", o.arg, o.start, o.end)
	case linSwap:
		return fmt.Sprintf("Swap(%d)=(%d,%v) [%d,%d]", o.arg, o.retV, o.retS, o.start, o.end)
	case linDelete:
		return fmt.Sprintf("Delete=(%d,%v) [%d,%d]", o.retV, o.retS, o.start, o.end)
	case linTryGet:
		return fmt.Sprintf("TryGet=(%d,%v) [%d,%d]", o.retV, o.retS, o.start, o.end)
	case linCAS:
		return fmt.Sprintf("CAS(%d,%d)=%v [%d,%d]", o.arg, o.arg2, o.retB, o.start, o.end)
	case linCAD:
		return fmt.Sprintf("CAD(%d)=%v [%d,%d]", o.arg, o.retB, o.start, o.end)
	}
	return "?"
}

type linState struct {
	present bool
	val     int
}

// linApply runs op against the sequential model state, reporting the next
// state and whether the operation's recorded returns are consistent with
// running at this point in the order.
func linApply(st linState, o *linOp) (linState, bool) {
	switch o.kind {
	case linSet:
		return linState{true, o.arg}, true
	case linSwap:
		if st.present {
			return linState{true, o.arg}, o.retS == Hit && o.retV == st.val
		}
		return linState{true, o.arg}, o.retS == Miss
	case linDelete:
		if st.present {
			return linState{}, o.retS == Hit && o.retV == st.val
		}
		return linState{}, o.retS == Miss
	case linTryGet:
		if st.present {
			return st, o.retS == Hit && o.retV == st.val
		}
		return st, o.retS == Miss
	case linCAS:
		if st.present && st.val == o.arg {
			return linState{true, o.arg2}, o.retB
		}
		return st, !o.retB
	case linCAD:
		if st.present && st.val == o.arg {
			return linState{}, o.retB
		}
		return st, !o.retB
	}
	panic("unreachable")
}

// linearizable reports whether some real-time-consistent total order of ops
// explains every op's returns starting from initial.
func linearizable(initial linState, ops []*linOp) bool {
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
			st2, ok := linApply(st, o)
			if !ok {
				continue
			}
			used[i] = true
			if rec(st2, depth+1) {
				return true
			}
			used[i] = false
		}
		return false
	}
	return rec(initial, 0)
}

func TestLinearizability(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short")
	}
	const (
		rounds   = 50_000
		opsPer   = 4
		valSpace = 3 // small domain so CAS/CAD old values plausibly match
	)
	c := New[int, int]() // shared across rounds so the trie grows/expands/prunes

	var ticket atomic.Int64
	for round := range rounds {
		k := round
		initial := linState{}
		if rand.IntN(2) == 1 {
			initial = linState{true, 1 + rand.IntN(valSpace)}
			c.Set(k, initial.val) // sequentially primed before the round
		}

		ops := make([]*linOp, opsPer)
		for i := range ops {
			ops[i] = &linOp{
				kind: linKind(rand.IntN(6)),
				arg:  1 + rand.IntN(valSpace),
				arg2: 1 + rand.IntN(valSpace),
			}
		}

		var wg sync.WaitGroup
		wg.Add(len(ops))
		for _, o := range ops {
			go func(o *linOp) {
				defer wg.Done()
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
				}
				o.end = ticket.Add(1)
			}(o)
		}
		wg.Wait()

		if !linearizable(initial, ops) {
			var dump strings.Builder
			for _, o := range ops {
				dump.WriteString("\n\t")
				dump.WriteString(o.String())
			}
			t.Fatalf("round %d: no linearization explains this history (initial present=%v val=%d):%s",
				round, initial.present, initial.val, dump.String())
		}
	}
}
