package cache

import (
	"errors"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

// Item delegates to Cache[struct{}, V]. These tests exercise every method so
// the thin wrappers contribute to coverage and so a refactor of the underlying
// Cache that forgets to propagate a method would be caught.

func TestItem_BasicGetAndCaches(t *testing.T) {
	i := NewItem[int]()
	var calls int32
	v, err, s := i.Get(func() (int, error) {
		atomic.AddInt32(&calls, 1)
		return 42, nil
	})
	if v != 42 || err != nil || !s.IsMiss() {
		t.Fatalf("first Get: got v=%d err=%v s=%v, want v=42 err=nil Miss", v, err, s)
	}
	v, err, s = i.Get(func() (int, error) {
		atomic.AddInt32(&calls, 1)
		return -1, nil
	})
	if v != 42 || err != nil || !s.IsHit() {
		t.Fatalf("second Get: got v=%d err=%v s=%v, want v=42 err=nil Hit", v, err, s)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("miss fn called %d times, want 1", got)
	}
}

func TestItem_TryGet(t *testing.T) {
	i := NewItem[string]()
	_, _, s := i.TryGet()
	if !s.IsMiss() {
		t.Fatalf("TryGet on empty item: state=%v want Miss", s)
	}
	i.Set("hello")
	v, err, s := i.TryGet()
	if v != "hello" || err != nil || !s.IsHit() {
		t.Fatalf("TryGet after Set: v=%q err=%v s=%v", v, err, s)
	}
}

func TestItem_Delete(t *testing.T) {
	i := NewItem[int]()
	i.Set(7)
	v, err, s := i.Delete()
	if v != 7 || err != nil || !s.IsHit() {
		t.Fatalf("Delete of set value: v=%d err=%v s=%v", v, err, s)
	}
	_, _, s = i.TryGet()
	if !s.IsMiss() {
		t.Fatalf("TryGet after Delete: state=%v want Miss", s)
	}
	// Deleting an already-empty item is a no-op.
	_, _, s = i.Delete()
	if !s.IsMiss() {
		t.Fatalf("Delete of empty item: state=%v want Miss", s)
	}
}

func TestItem_Expire(t *testing.T) {
	i := NewItem[int](MaxAge(time.Hour))
	i.Set(99)
	if _, _, s := i.TryGet(); !s.IsHit() {
		t.Fatalf("before Expire: want Hit")
	}
	i.Expire()
	if _, _, s := i.TryGet(); !s.IsMiss() {
		t.Fatalf("after Expire: want Miss")
	}
}

func TestItem_Swap(t *testing.T) {
	i := NewItem[int]()
	old, err, s := i.Swap(5)
	if err != nil || !s.IsMiss() {
		t.Fatalf("first Swap: old=%d err=%v s=%v, want Miss", old, err, s)
	}
	old, err, s = i.Swap(6)
	if old != 5 || err != nil || !s.IsHit() {
		t.Fatalf("second Swap: old=%d err=%v s=%v, want 5 Hit", old, err, s)
	}
	if v, _, _ := i.TryGet(); v != 6 {
		t.Fatalf("after second Swap: TryGet=%d, want 6", v)
	}
}

func TestItem_CompareAndSwap(t *testing.T) {
	i := NewItem[int]()
	i.Set(10)
	if !i.CompareAndSwap(10, 11) {
		t.Fatal("CAS 10->11 should succeed")
	}
	if i.CompareAndSwap(10, 12) {
		t.Fatal("CAS 10->12 should fail after value is 11")
	}
	if v, _, _ := i.TryGet(); v != 11 {
		t.Fatalf("after CAS: TryGet=%d, want 11", v)
	}
}

func TestItem_CompareAndDelete(t *testing.T) {
	i := NewItem[int]()
	i.Set(7)
	if i.CompareAndDelete(8) {
		t.Fatal("CAD of wrong value should fail")
	}
	if !i.CompareAndDelete(7) {
		t.Fatal("CAD of correct value should succeed")
	}
	if _, _, s := i.TryGet(); !s.IsMiss() {
		t.Fatalf("after CAD: state=%v want Miss", s)
	}
}

func TestItem_Clear(t *testing.T) {
	i := NewItem[int]()
	i.Set(1)
	i.Clear()
	if _, _, s := i.TryGet(); !s.IsMiss() {
		t.Fatalf("after Clear: state=%v want Miss", s)
	}
}

func TestItem_ZeroValue(t *testing.T) {
	// Docstring says zero-value Item is valid.
	var i Item[int]
	v, err, s := i.Get(func() (int, error) { return 3, nil })
	if v != 3 || err != nil || !s.IsMiss() {
		t.Fatalf("zero Item Get: v=%d err=%v s=%v", v, err, s)
	}
	if v, _, _ := i.TryGet(); v != 3 {
		t.Fatalf("zero Item TryGet: v=%d want 3", v)
	}
}

// Set is a key-only cache over Cache[K, struct{}].

func TestSet_BasicGet(t *testing.T) {
	s := NewSet[string]()
	var calls int32
	err, ks := s.Get("a", func() error {
		atomic.AddInt32(&calls, 1)
		return nil
	})
	if err != nil || !ks.IsMiss() {
		t.Fatalf("first Get: err=%v state=%v want nil Miss", err, ks)
	}
	err, ks = s.Get("a", func() error {
		atomic.AddInt32(&calls, 1)
		return nil
	})
	if err != nil || !ks.IsHit() {
		t.Fatalf("second Get: err=%v state=%v want nil Hit", err, ks)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("miss fn called %d times, want 1", got)
	}
}

func TestSet_GetError(t *testing.T) {
	s := NewSet[string]()
	wantErr := errors.New("boom")
	err, ks := s.Get("a", func() error { return wantErr })
	if !errors.Is(err, wantErr) || !ks.IsMiss() {
		t.Fatalf("Get with error: err=%v state=%v", err, ks)
	}
}

func TestSet_TryGet(t *testing.T) {
	s := NewSet[string]()
	_, ks := s.TryGet("missing")
	if !ks.IsMiss() {
		t.Fatalf("TryGet missing: state=%v", ks)
	}
	s.Set("k")
	err, ks := s.TryGet("k")
	if err != nil || !ks.IsHit() {
		t.Fatalf("TryGet after Set: err=%v state=%v", err, ks)
	}
}

func TestSet_Delete(t *testing.T) {
	s := NewSet[string]()
	s.Set("k")
	_, ks := s.Delete("k")
	if !ks.IsHit() {
		t.Fatalf("Delete existing: state=%v want Hit", ks)
	}
	_, ks = s.Delete("k")
	if !ks.IsMiss() {
		t.Fatalf("Delete missing: state=%v want Miss", ks)
	}
}

func TestSet_Expire(t *testing.T) {
	s := NewSet[string](MaxAge(time.Hour))
	s.Set("k")
	s.Expire("k")
	if _, ks := s.TryGet("k"); !ks.IsMiss() {
		t.Fatalf("TryGet after Expire: state=%v want Miss", ks)
	}
}

func TestSet_Range(t *testing.T) {
	s := NewSet[string]()
	for _, k := range []string{"a", "b", "c"} {
		s.Set(k)
	}
	var got []string
	s.Range(func(k string, err error) bool {
		if err != nil {
			t.Errorf("unexpected err for key %q: %v", k, err)
		}
		got = append(got, k)
		return true
	})
	sort.Strings(got)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("Range yielded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Range yielded %v, want %v", got, want)
		}
	}

	// Early stop.
	count := 0
	s.Range(func(string, error) bool {
		count++
		return false
	})
	if count != 1 {
		t.Fatalf("Range early-stop visited %d, want 1", count)
	}
}

func TestSet_Clean(t *testing.T) {
	s := NewSet[string](MaxAge(time.Nanosecond))
	s.Set("k")
	time.Sleep(time.Millisecond)
	s.Clean()
	if _, ks := s.TryGet("k"); !ks.IsMiss() {
		t.Fatalf("TryGet after Clean on expired: state=%v want Miss", ks)
	}
}

func TestSet_Clear(t *testing.T) {
	s := NewSet[string]()
	s.Set("a")
	s.Set("b")
	s.Clear()
	if _, ks := s.TryGet("a"); !ks.IsMiss() {
		t.Fatalf("after Clear: state=%v want Miss", ks)
	}
	// Range yields nothing.
	var got []string
	s.Range(func(k string, _ error) bool {
		got = append(got, k)
		return true
	})
	if len(got) != 0 {
		t.Fatalf("Range after Clear: %v, want empty", got)
	}
}

func TestSet_StopAutoClean(t *testing.T) {
	s := NewSet[string](MaxAge(time.Millisecond), AutoCleanInterval(time.Millisecond))
	s.StopAutoClean()
	// Calling twice is safe (sync.Once on the underlying Cache).
	s.StopAutoClean()
}

func TestSet_ZeroValue(t *testing.T) {
	var s Set[string]
	err, ks := s.Get("a", func() error { return nil })
	if err != nil || !ks.IsMiss() {
		t.Fatalf("zero Set Get: err=%v state=%v", err, ks)
	}
}
