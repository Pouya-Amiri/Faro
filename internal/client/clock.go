package client

import (
	"sort"
	"sync"
	"time"
)

type clockSample struct {
	rtt    time.Duration
	offset time.Duration
}

type clockEstimator struct {
	mu      sync.RWMutex
	samples []clockSample
}

func (c *clockEstimator) observe(clientSent, serverReceived, clientReceived int64) {
	rttMillis := clientReceived - clientSent
	if rttMillis < 0 || rttMillis > 30_000 {
		return
	}
	midpoint := clientSent + rttMillis/2
	sample := clockSample{
		rtt:    time.Duration(rttMillis) * time.Millisecond,
		offset: time.Duration(serverReceived-midpoint) * time.Millisecond,
	}
	c.mu.Lock()
	c.samples = append(c.samples, sample)
	if len(c.samples) > 12 {
		c.samples = append([]clockSample(nil), c.samples[len(c.samples)-12:]...)
	}
	c.mu.Unlock()
}

func (c *clockEstimator) offset() time.Duration {
	c.mu.RLock()
	samples := append([]clockSample(nil), c.samples...)
	c.mu.RUnlock()
	if len(samples) == 0 {
		return 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].rtt < samples[j].rtt })
	count := 3
	if len(samples) < count {
		count = len(samples)
	}
	var total time.Duration
	for _, sample := range samples[:count] {
		total += sample.offset
	}
	return total / time.Duration(count)
}
