package eventloop

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ErrQueueBlocked is returned when a new item should be added to the queue
// but it is currently blocked
var ErrQueueBlocked = errors.New("queue currently blocked")

type node struct {
	data Task
	next *node
}

// Queue implements a job queue for the event loop
type Queue struct {
	name     string
	loopName string
	head     *node
	tail     *node
	count    int
	lock     *sync.Mutex
	blocked  bool
	waitCh   chan struct{}
}

// NewQueue creates a new queue
func NewQueue(loopName, name string) *Queue {
	return &Queue{
		lock:     &sync.Mutex{},
		waitCh:   make(chan struct{}),
		name:     name,
		loopName: loopName,
	}
}

// Block the queue and deny any push action. Pop will still work
func (q *Queue) Block() {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.blocked = true
}

// Unblock unblocks the queue and allows new items to be added
func (q *Queue) Unblock() {
	q.lock.Lock()
	defer q.lock.Unlock()

	q.blocked = true
}

// Len returns the number of jobs queued
func (q *Queue) Len() int {
	q.lock.Lock()
	defer q.lock.Unlock()

	return q.count
}

// Push pushes a new job onto the queue
func (q *Queue) Push(item Task) error {
	q.lock.Lock()
	defer q.lock.Unlock()

	if q.blocked {
		return ErrQueueBlocked
	}

	n := &node{data: item}
	if q.tail == nil {
		q.tail = n
		q.head = n
	} else {
		q.tail.next = n
		q.tail = n
	}
	q.count++
	totalJobs.With(prometheus.Labels{"loop": q.loopName, "queue": q.name}).Inc()
	queuedJobs.With(prometheus.Labels{"loop": q.loopName, "queue": q.name}).Inc()

	// if there's someone waiting on PopWait(),
	// try to notify it
	select {
	case q.waitCh <- struct{}{}:
	default:
	}

	return nil
}

// Pop returns the next task to execute from the queue or nil
// if the queue is empty
func (q *Queue) Pop() Task {
	q.lock.Lock()
	defer q.lock.Unlock()

	if q.head == nil {
		return nil
	}

	n := q.head
	q.head = n.next

	if q.head == nil {
		q.tail = nil
	}
	q.count--
	queuedJobs.With(prometheus.Labels{"loop": q.loopName, "queue": q.name}).Dec()

	return n.data
}

// PopWait returns the next job from the queue and will
// block until either the context is cancelled or a job
// becomes available. If the queue is empty and blocked
// (i.e. Block() has been called), PopWait will return
// immediately with ErrQueueBlocked. If the provided context
// is canceled ctx.Err() will be returned
func (q *Queue) PopWait(ctx context.Context) (Task, error) {
	next := q.Pop()
	if next == nil {
		q.lock.Lock()
		blocked := q.blocked
		q.lock.Unlock()

		if blocked {
			return nil, ErrQueueBlocked
		}

		start := time.Now()

		defer func() {
			queueIdle.With(prometheus.Labels{"queue": q.name, "loop": q.loopName}).Add(float64(time.Now().Sub(start).Seconds()))
		}()

		select {
		case <-q.waitCh:
			return q.PopWait(ctx)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return next, nil
}
