package ordermanager

import (
	"sync"

	"github.com/nkalva/kitealgo/pkg/models"
)

// PriorityQueue is a thread-safe multi-lane FIFO. Pop always serves the
// highest-priority non-empty lane; within a lane, order is preserved.
//
// A single Go select over several channels picks uniformly at random among
// ready cases, so "priority channels" built from plain selects do not actually
// prioritise under load. Explicit lanes behind a mutex do.
type PriorityQueue struct {
	mu     sync.Mutex
	lanes  [models.NumPriorities][]*models.OrderPayload
	signal chan struct{}
	high   [models.NumPriorities]int // high-water marks for monitoring
}

// NewPriorityQueue returns an empty queue.
func NewPriorityQueue() *PriorityQueue {
	return &PriorityQueue{signal: make(chan struct{}, 1)}
}

// Push enqueues p and wakes the consumer.
func (q *PriorityQueue) Push(p *models.OrderPayload) {
	q.mu.Lock()
	l := int(p.Priority)
	if l < 0 || l >= models.NumPriorities {
		l = int(models.PriorityEntry)
	}
	q.lanes[l] = append(q.lanes[l], p)
	if n := len(q.lanes[l]); n > q.high[l] {
		q.high[l] = n
	}
	q.mu.Unlock()
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

// Pop removes the highest-priority item, or returns nil if empty.
func (q *PriorityQueue) Pop() *models.OrderPayload {
	q.mu.Lock()
	defer q.mu.Unlock()
	for l := range q.lanes {
		if len(q.lanes[l]) > 0 {
			p := q.lanes[l][0]
			q.lanes[l][0] = nil // release for GC
			q.lanes[l] = q.lanes[l][1:]
			if len(q.lanes[l]) == 0 {
				q.lanes[l] = nil // drop the backing array
			}
			return p
		}
	}
	return nil
}

// Len returns the total queued items.
func (q *PriorityQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for l := range q.lanes {
		n += len(q.lanes[l])
	}
	return n
}

// Drain removes and returns every item in lane p.
func (q *PriorityQueue) Drain(p models.Priority) []*models.OrderPayload {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.lanes[p]
	q.lanes[p] = nil
	return out
}

// Signal fires (coalesced) whenever something is pushed.
func (q *PriorityQueue) Signal() <-chan struct{} { return q.signal }

// HighWater returns per-lane maximum depths seen.
func (q *PriorityQueue) HighWater() [models.NumPriorities]int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.high
}
