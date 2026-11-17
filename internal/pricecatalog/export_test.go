package pricecatalog

import (
	"context"
	"time"
)

// Tick runs one scheduler step (tests).
func Tick(m *Manager, ctx context.Context, jitter time.Duration) { m.tick(ctx, jitter) }
