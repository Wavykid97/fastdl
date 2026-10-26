package main

import (
	"context"
	"sync"
	"time"
)

// Limiter caps the total download speed across every connection of every
// download. A rate of 0 means no limit. The rate can be changed at any time.
type Limiter struct {
	mu   sync.Mutex
	rate float64   // bytes per second
	next time.Time // when the next byte is "allowed"
}

func (l *Limiter) SetRate(bytesPerSec int64) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.rate = float64(bytesPerSec)
	l.next = time.Now()
	l.mu.Unlock()
}

func (l *Limiter) Rate() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return int64(l.rate)
}

// Wait accounts for n bytes just received, sleeping as needed to stay
// under the limit. Slowing down reading makes the server slow down too.
func (l *Limiter) Wait(ctx context.Context, n int) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if l.rate <= 0 {
		l.mu.Unlock()
		return nil
	}
	now := time.Now()
	// Allow a small burst (a quarter second's worth) so things stay smooth.
	if floor := now.Add(-250 * time.Millisecond); l.next.Before(floor) {
		l.next = floor
	}
	l.next = l.next.Add(time.Duration(float64(n) / l.rate * float64(time.Second)))
	wait := l.next.Sub(now)
	l.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
