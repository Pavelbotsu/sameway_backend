package handlers

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// runLimited never lets more than cap(sem) background runs execute at once,
// and drops a run that can't get a slot in time instead of piling up.
func TestRunLimited_CapsConcurrency(t *testing.T) {
	sem := make(chan struct{}, 2)
	var running, peak int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runLimited(sem, time.Second, "test", func() {
				n := atomic.AddInt32(&running, 1)
				for {
					p := atomic.LoadInt32(&peak)
					if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				atomic.AddInt32(&running, -1)
			})
		}()
	}
	wg.Wait()
	if peak > 2 {
		t.Fatalf("peak concurrency %d, want ≤ 2", peak)
	}
}

func TestRunLimited_DropsWhenSaturated(t *testing.T) {
	sem := make(chan struct{}, 1)
	sem <- struct{}{} // slot taken
	ran := runLimited(sem, 20*time.Millisecond, "test", func() { t.Fatal("must not run") })
	if ran {
		t.Fatal("expected the run to be skipped")
	}
}
