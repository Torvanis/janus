package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// doUpstream sends one proxied request. A non-streamed request keeps the
// client's total timeout: the whole exchange must finish within it. A
// streamed request is instead cut off only after `idle` of silence — no byte
// from the upstream for that long — so a model that streams steadily for
// longer than the total timeout (long reasoning, a long document) finishes.
// The connect and time-to-first-byte limits apply either way.
func doUpstream(client *http.Client, req *http.Request, streaming bool, idle time.Duration) (*http.Response, error) {
	if !streaming || client.Timeout <= 0 {
		return client.Do(req)
	}
	c := *client
	c.Timeout = 0
	ctx, cancel := context.WithCancel(req.Context())
	b := &idleBody{idle: idle, cancel: cancel}
	// The silence clock runs from the moment the request is sent; the
	// transport's time-to-first-byte limit normally fires first.
	b.timer = time.AfterFunc(idle, b.expire)
	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		b.timer.Stop()
		cancel()
		if b.fired.Load() {
			return nil, b.silenceErr()
		}
		return nil, err
	}
	b.rc = resp.Body
	resp.Body = b
	return resp, nil
}

// idleBody aborts a streamed upstream body after `idle` with no data.
type idleBody struct {
	rc     io.ReadCloser
	idle   time.Duration
	timer  *time.Timer
	cancel context.CancelFunc
	fired  atomic.Bool
	once   sync.Once
}

func (b *idleBody) expire() {
	b.fired.Store(true)
	b.cancel()
}

func (b *idleBody) silenceErr() error {
	return fmt.Errorf("the upstream sent nothing for %s", b.idle.Round(time.Second))
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 && !b.fired.Load() {
		b.timer.Reset(b.idle)
	}
	if err != nil && err != io.EOF && b.fired.Load() {
		return n, b.silenceErr()
	}
	return n, err
}

func (b *idleBody) Close() error {
	var err error
	b.once.Do(func() {
		b.timer.Stop()
		err = b.rc.Close()
		b.cancel()
	})
	return err
}
