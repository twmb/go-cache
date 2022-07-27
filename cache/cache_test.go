package cache

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type got[V any] struct {
	v   V
	err error
	s   KeyState
}

func vcheck[V comparable](t *testing.T, g, e got[V]) {
	t.Helper()
	if g.v != e.v {
		t.Errorf("got %v != exp %v", g.v, e.v)
	}
	if gotErr, expErr := g.err != nil, e.err != nil; gotErr != expErr {
		t.Errorf("got err? %v (%v) != exp err? %v (%v)", gotErr, g.err, expErr, e.err)
	}
	if g.s != e.s {
		t.Errorf("got key state? %v != exp key state? %v", g.s, e.s)
	}
}

func TestGet2x(t *testing.T) {
	var c Cache[string, int]

	{
		i, _, _ := c.Get("foo", func() (int, error) {
			return 3, nil
		})
		if i != 3 {
			t.Errorf("got %d != exp 3", i)
		}
	}

	{
		i, _, _ := c.Get("foo", func() (int, error) {
			panic("should have been cached")
		})
		if i != 3 {
			t.Errorf("got %d != exp 3", i)
		}
	}
}

func TestCollapsedGet(t *testing.T) {
	const niter = 1000

	var (
		c     Cache[string, *int]
		r     = new(int)
		calls int64
		ps    = make(chan *int)
	)
	for i := 0; i < niter; i++ {
		go func() {
			p, _, _ := c.Get("foo", func() (*int, error) {
				if atomic.AddInt64(&calls, 1) != 1 {
					t.Error("closure called multiple times")
				}
				time.Sleep(50 * time.Millisecond)
				return r, nil
			})
			ps <- p
		}()
	}
	for i := 0; i < niter; i++ {
		p := <-ps
		if p != r {
			t.Error("pointer mismatch")
		}
	}
}

func TestSetExpire(t *testing.T) {
	var c Cache[string, int]

	k := "a"

	check := func(got got[int], exp int) {
		if got.v != exp {
			t.Errorf("got %d != exp %d", got.v, exp)
		}
		if got.err != nil {
			t.Errorf("unexpected err: %v", got.err)
		}
	}

	ch := make(chan struct{})
	vch := make(chan got[int], 1)
	go func() {
		v, err, s := c.Get(k, func() (int, error) {
			ch <- struct{}{}
			<-ch
			return 0, errors.New("foo")
		})
		vch <- got[int]{v, err, s}
	}()

	<-ch
	c.Set(k, 4)
	ch <- struct{}{}
	check(<-vch, 4)
	v, err, s := c.Get(k, func() (int, error) { panic("unreachable") })
	check(got[int]{v, err, s}, 4)

	c.Set(k, 10)
	v, err, s = c.Get(k, func() (int, error) { panic("unreachable") })
	check(got[int]{v, err, s}, 10)

	c.Range(func(string, int, error) bool { return false })
	c.Delete(k)
	c.Set(k, 10)

	for i := 0; i < 2; i++ {
		var wg sync.WaitGroup
		wg.Add(10)
		go func() { defer wg.Done(); c.Range(func(string, int, error) bool { return false }) }()
		go func() { defer wg.Done(); c.Delete(k) }()
		go func() { defer wg.Done(); c.Set(k, 10) }()
		go func() { defer wg.Done(); c.Expire(k) }()
		go func() { defer wg.Done(); c.Set(k, 10) }()
		go func() { defer wg.Done(); c.Range(func(string, int, error) bool { return false }) }()
		go func() { defer wg.Done(); c.Delete(k) }()
		go func() { defer wg.Done(); c.Set(k, 10) }()
		go func() { defer wg.Done(); c.Expire(k) }()
		go func() { defer wg.Done(); c.Set(k, 10) }()
		wg.Wait()
		v, err, s = c.Get(k, func() (int, error) { return 10, nil })
		check(got[int]{v, err, s}, 10)
	}

	c.Expire("asdf")
}

func TestExpires(t *testing.T) {
	// Expire things immediately: we expect all gets to be misses.
	{
		c := New[string, string](
			MaxAge(time.Nanosecond),
		)

		v, err, s := c.Get("foo", func() (string, error) {
			return "bar", nil
		})
		vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Miss})

		v, err, s = c.Get("foo", func() (string, error) {
			return "baz", nil
		})
		vcheck(t, got[string]{v, err, s}, got[string]{"baz", nil, Miss})
	}

	// Expire immediately, but have stales.
	{
		c := New[string, string](
			MaxAge(time.Nanosecond),
			MaxStaleAge(24*time.Hour),
		)

		c.Get("foo", func() (string, error) { return "bar", nil }) // prime the stale

		// Here, we check that the stale is returned while we load.
		ch := make(chan struct{})
		v, err, s := c.Get("foo", func() (string, error) {
			<-ch
			return "baz", nil
		})
		close(ch)
		vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Stale})

		// Here, we check that the stale is returned because it is
		// expired.  We sleep 10ms to allow the goroutines running the
		// prior miss to finish and update our value to baz.
		time.Sleep(10 * time.Millisecond)
		v, err, s = c.Get("foo", func() (string, error) {
			return "bar", nil
		})
		vcheck(t, got[string]{v, err, s}, got[string]{"baz", nil, Stale})
	}

	// Return stale while value is an error.
	{
		c := New[string, string](
			MaxStaleAge(-1),
		)
		c.Get("foo", func() (string, error) { return "bar", nil })
		time.Sleep(time.Millisecond)
		c.Expire("foo")
		time.Sleep(time.Millisecond)
		c.Get("foo", func() (string, error) { return "", errors.New("foo") })
		time.Sleep(time.Millisecond)

		// The cache is primed: foo exists, it was expired, and we
		// injected an error such that the next loads should be stale.

		v, err, s := c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Stale})

		ch := make(chan struct{})
		v, err, s = c.Get("foo", func() (string, error) {
			<-ch
			return "baz", nil
		})
		vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Stale})

		v, err, s = c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Stale})
		ch <- struct{}{}
	}

	// Misc. bits.
	{
		c := New[string, string](MaxStaleAge(-1))
		c.Get("foo", func() (string, error) { return "bar", nil })
		c.Range(func(string, string, error) bool { return true })
		c.Delete("foo")
		v, err, s := c.Get("foo", func() (string, error) { return "baz", nil })
		vcheck(t, got[string]{v, err, s}, got[string]{"baz", nil, Miss})
	}

	// Set a stale value, and get it.
	{
		c := New[string, string](MaxStaleAge(-1))
		c.Set("foo", "biz")
		c.Expire("foo")
		v, err, s := c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"biz", nil, Stale})
	}

	// Stale value expiry.
	{
		// We expire foo, and it is not used even though it is a stale
		// value.
		c := New[string, string](MaxStaleAge(1))
		c.Set("foo", "bar")
		c.Expire("foo")
		v, err, s := c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"", nil, Miss})
		v, err, s = c.Get("foo", func() (string, error) { return "baz", nil })
		vcheck(t, got[string]{v, err, s}, got[string]{"baz", nil, Miss})

		// TryGet returns immediately with miss if we are loading and
		// stale is expired.
		c.Expire("foo")
		ch := make(chan struct{})
		go func() {
			defer close(ch)
			c.Get("foo", func() (string, error) {
				ch <- struct{}{}
				<-ch
				return "asdf", nil
			})
		}()
		<-ch
		v, err, s = c.TryGet("foo")
		ch <- struct{}{}
		vcheck(t, got[string]{v, err, s}, got[string]{"", nil, Miss})

		// We expire foo, and return an error. The error is returned
		// because our stale is expired.
		<-ch
		c.Expire("foo")
		c.Get("foo", func() (string, error) { return "", errors.New("err") })
		v, err, s = c.Get("foo", func() (string, error) { panic("unreachable") })
		vcheck(t, got[string]{v, err, s}, got[string]{"", errors.New("err"), Hit})
	}

	// Values deleted immediately.
	{
		c := New[string, string](MaxAge(0))

		gch := make(chan got[string], 10)
		ch := make(chan struct{})
		for i := 0; i < 10; i++ {
			go func() {
				v, err, s := c.Get("foo", func() (string, error) {
					<-ch
					return "bar", nil
				})
				gch <- got[string]{v, err, s}
			}()
		}

		// We give the Get's a chance to stack, but we do not sleep
		// long, so not all Get's could be stacked at once.
		time.Sleep(100 * time.Millisecond)
		close(ch)
		for i := 0; i < 10; i++ {
			g := <-gch
			if g.s == Miss {
				vcheck(t, g, got[string]{"bar", nil, Miss})
			} else {
				vcheck(t, g, got[string]{"bar", nil, Hit})
			}
		}

		// We expect a miss because nothing persists.
		v, err, s := c.TryGet("foo")
		vcheck(t, got[string]{v, err, s}, got[string]{"", nil, Miss})

	}

	// Errors are deleted immediately.
	{
		c := New[string, string](MaxErrorAge(1))
		v, err, s := c.Get("foo", func() (string, error) { return "", errors.New("foo") })
		vcheck(t, got[string]{v, err, s}, got[string]{"", errors.New("foo"), Miss})
		v, err, s = c.Get("foo", func() (string, error) { return "bar", nil })
		vcheck(t, got[string]{v, err, s}, got[string]{"bar", nil, Miss})
	}

	// Clean expired values.
	{
		c := New[string, string]()
		c.Set("1", "foo")
		c.Set("2", "foo")
		c.Set("3", "foo")
		c.Set("4", "foo")
		c.Set("5", "foo")
		c.Set("6", "foo")
		c.Set("7", "foo")
		c.Set("8", "foo")
		c.Set("9", "foo")
		c.Expire("1")
		c.Expire("3")
		c.Expire("5")
		c.Expire("7")
		c.Expire("9")
		c.Clean()

		exp := map[string]bool{
			"2": true,
			"4": true,
			"6": true,
			"8": true,
		}
		c.Range(func(k, v string, err error) bool {
			if !exp[k] {
				t.Errorf("unexpected key %s remaining", k)
			}
			delete(exp, k)
			if v != "foo" {
				t.Errorf("unexpected value %s", v)
			}
			if err != nil {
				t.Errorf("unexpected err %v", err)
			}
			return true
		})
		if len(exp) != 0 {
			t.Errorf("unexpected values remain: %v", exp)
		}
	}
}
