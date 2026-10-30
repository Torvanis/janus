package httpapi

import "sync"

// parallel runs independent read queries concurrently and returns the first
// error. Dashboards issue several aggregate queries over the same range;
// serially their latencies add up, concurrently the page waits for the
// slowest one. Each fn must only touch its own result variable.
func parallel(fns ...func() error) error {
	var wg sync.WaitGroup
	errs := make([]error, len(fns))
	for i, fn := range fns {
		wg.Add(1)
		go func(i int, fn func() error) {
			defer wg.Done()
			errs[i] = fn()
		}(i, fn)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
