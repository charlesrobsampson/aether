// Package dispatch owns Aether's bounded inbound dispatch queue.
//
// /receive validates and submits a message; a single worker calls the handler
// asynchronously. Submission never blocks: a full queue returns ErrFull so the
// transport can retry instead of pinning its webhook.
package dispatch

import (
	"context"
	"errors"
	"sync"

	"github.com/charlesrobsampson/aether/internal/model"
)

const defaultDepth = 256

// ErrFull reports that the bounded queue could not accept another message.
var ErrFull = errors.New("dispatch: queue is full")

// ErrClosed reports that Shutdown has already stopped the queue.
var ErrClosed = errors.New("dispatch: queue is closed")

// ErrNoHandler reports a missing dispatch handler at construction time.
var ErrNoHandler = errors.New("dispatch: handler is required")

// Handler processes one accepted inbound message. Errors are not returned: the
// router audits every outcome in route_log.
type Handler func(context.Context, model.Inbound)

// Options configures a Queue.
type Options struct {
	// Depth bounds the number of buffered messages. A non-positive value
	// selects the default (256).
	Depth int
	// Handler runs one accepted message. It is required.
	Handler Handler
}

// Queue is a bounded, single-worker dispatch queue.
type Queue struct {
	jobs    chan model.Inbound
	handler Handler
	ctx     context.Context
	cancel  context.CancelFunc

	mu       sync.Mutex
	closed   bool
	forceOne sync.Once
	wg       sync.WaitGroup

	forceStop chan struct{}
}

// New builds a queue and starts its worker.
func New(opts Options) (*Queue, error) {
	if opts.Handler == nil {
		return nil, ErrNoHandler
	}
	depth := opts.Depth
	if depth <= 0 {
		depth = defaultDepth
	}

	ctx, cancel := context.WithCancel(context.Background())
	q := &Queue{
		jobs:      make(chan model.Inbound, depth),
		handler:   opts.Handler,
		ctx:       ctx,
		cancel:    cancel,
		forceStop: make(chan struct{}),
	}
	q.wg.Add(1)
	go q.run()
	return q, nil
}

// Submit offers one message without blocking. It returns ErrFull when the
// buffer is full and ErrClosed after Shutdown.
func (q *Queue) Submit(ctx context.Context, in model.Inbound) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrClosed
	}

	select {
	case q.jobs <- in:
		return nil
	default:
		return ErrFull
	}
}

// Shutdown stops accepting messages, drains the buffer, and waits for the
// worker. If ctx expires first, the worker's context is cancelled and any
// remaining buffered messages are dropped; Shutdown still returns promptly.
func (q *Queue) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.jobs)
	}
	q.mu.Unlock()

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		q.cancel()
		return nil
	case <-ctx.Done():
		q.forceOne.Do(func() {
			close(q.forceStop)
			q.cancel()
		})
		<-done
		return ctx.Err()
	}
}

func (q *Queue) run() {
	defer q.wg.Done()
	for job := range q.jobs {
		select {
		case <-q.forceStop:
			// A forced shutdown abandons buffered work; the handler below
			// would immediately fail on the cancelled context anyway.
			continue
		default:
		}
		q.handler(q.ctx, job)
	}
}
