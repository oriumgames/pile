package format

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// parallelFor runs fn(i) for i in [0, n) across at most `workers` goroutines
// (0 = GOMAXPROCS). Small inputs and workers == 1 run inline.
func parallelFor(n, workers int, fn func(i int)) {
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers > n {
		workers = n
	}
	if n <= 1 || workers <= 1 {
		for i := range n {
			fn(i)
		}
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				fn(i)
			}
		}()
	}
	wg.Wait()
}
