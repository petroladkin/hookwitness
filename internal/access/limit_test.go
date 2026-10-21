package access

import (
	"sync"
	"testing"
	"time"
)

// A burst cannot pass the limit: an allowed attempt counts until it is
// settled, so however many arrive at once, at most MaxFailures are checked.
func TestLimiterBurst(t *testing.T) {
	l := NewLimiter(nil)
	var mu sync.Mutex
	allowed := 0
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Allow("192.0.2.7"); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
				time.Sleep(10 * time.Millisecond) // the password check
				l.Fail("192.0.2.7")
			}
		}()
	}
	wg.Wait()
	if allowed != MaxFailures {
		t.Fatalf("%d attempts passed, want %d", allowed, MaxFailures)
	}
	if ok, wait := l.Allow("192.0.2.7"); ok || wait < 14*time.Minute {
		t.Fatalf("after the burst: %v %v", ok, wait)
	}
	// A released attempt (the server was busy) gives its place back.
	if ok, _ := l.Allow("192.0.2.8"); !ok {
		t.Fatal("a fresh address refused")
	}
	l.Release("192.0.2.8")
	for range MaxFailures {
		if ok, _ := l.Allow("192.0.2.8"); !ok {
			t.Fatal("a released attempt kept its place")
		}
	}
}
