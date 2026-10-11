package limit

import (
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func newAt(r Rate, max int, c *clock) *Limiter {
	l := New(r, max)
	l.Now = c.now
	return l
}

func TestBurstThenRefill(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	l := newAt(Rate{PerMinute: 60, Burst: 3}, 0, c)
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d refused inside the burst", i)
		}
	}
	ok, wait := l.Allow("a")
	if ok || wait != time.Second {
		t.Fatalf("over the burst: ok %v, wait %v; want refused, 1s", ok, wait)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("another key shares the first's bucket")
	}
	c.add(time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("not refilled after a second at 60/min")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("refilled more than one")
	}
	c.add(time.Hour)
	for i := 0; i < 3; i++ {
		l.Allow("a")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("an idle hour banked more than the burst")
	}
}

// Memory stays bounded: refilled buckets go first, and a key in debt is
// never evicted to make room, which would reset its penalty.
func TestMaxKeys(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	l := newAt(Rate{PerMinute: 1, Burst: 1}, 2, c)
	l.Allow("a")
	l.Allow("b")
	if ok, _ := l.Allow("c"); !ok || !l.Full() {
		t.Fatal("a new key over MaxKeys with every bucket in debt should pass, and Full say so")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("a lost its debt to make room")
	}
	c.add(time.Minute)
	if ok, _ := l.Allow("c"); !ok || l.Full() {
		t.Fatal("refilled buckets were not swept for a new key")
	}
	if len(l.buckets) > 2 {
		t.Fatalf("%d buckets, MaxKeys 2", len(l.buckets))
	}
}
