package metrics

import (
	"sync"
	"sync/atomic"
	"time"
)

// Collector tracks live traffic and connection counters.
type Collector struct {
	bytesUp   atomic.Uint64 // client -> remote (upload)
	bytesDown atomic.Uint64 // remote -> client (download)
	conns     atomic.Int64
	connTotal atomic.Uint64

	mu       sync.Mutex
	prevUp   uint64
	prevDown uint64
	prevAt   time.Time
	upRate   float64 // bytes/sec
	downRate float64
}

func New() *Collector {
	return &Collector{prevAt: time.Now()}
}

func (c *Collector) AddUp(n int64) {
	if n > 0 {
		c.bytesUp.Add(uint64(n))
	}
}

func (c *Collector) AddDown(n int64) {
	if n > 0 {
		c.bytesDown.Add(uint64(n))
	}
}

func (c *Collector) ConnOpen() {
	c.conns.Add(1)
	c.connTotal.Add(1)
}

func (c *Collector) ConnClose() {
	for {
		cur := c.conns.Load()
		if cur <= 0 {
			c.conns.Store(0)
			return
		}
		if c.conns.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

type Snapshot struct {
	BytesUp    uint64
	BytesDown  uint64
	UpRate     float64
	DownRate   float64
	Conns      int64
	ConnsTotal uint64
}

func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	up := c.bytesUp.Load()
	down := c.bytesDown.Load()
	dt := now.Sub(c.prevAt).Seconds()
	if dt >= 0.2 {
		c.upRate = float64(up-c.prevUp) / dt
		c.downRate = float64(down-c.prevDown) / dt
		c.prevUp = up
		c.prevDown = down
		c.prevAt = now
	}
	return Snapshot{
		BytesUp:    up,
		BytesDown:  down,
		UpRate:     c.upRate,
		DownRate:   c.downRate,
		Conns:      c.conns.Load(),
		ConnsTotal: c.connTotal.Load(),
	}
}
