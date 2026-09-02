package logrotate

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

const (
	// retainedErrorLimit bounds the journal returned by Shutdown: the first
	// error plus the most recent retainedErrorLimit-1, the rest counted.
	retainedErrorLimit = 16
	// errorNoticeBuffer is how many notifications may queue for a slow
	// WithErrorHandler callback before further ones are dropped and counted.
	errorNoticeBuffer = 16
)

// errorState is the Writer's record of maintenance errors. It separates the
// durable, bounded journal that Shutdown returns from the best-effort
// notification of a WithErrorHandler callback, which runs serially on its
// own goroutine and never under a Writer or journal lock, so a slow or
// misbehaving handler cannot stall writes or maintenance.
type errorState struct {
	mu      sync.Mutex
	first   error
	recent  []error
	omitted uint64

	handler   func(error)
	notices   chan error
	dropped   atomic.Uint64
	closeOnce sync.Once
}

// newErrorState creates the journal and, when a handler is configured, the
// notification queue; start begins delivery.
func newErrorState(handler func(error)) *errorState {
	s := &errorState{handler: handler}
	if handler != nil {
		s.notices = make(chan error, errorNoticeBuffer)
	}
	return s
}

// start launches the delivery goroutine when a handler is configured.
func (s *errorState) start() {
	if s.notices != nil {
		go s.deliver()
	}
}

// report journals err and, when a handler is configured, queues a
// notification without blocking; an overflowing queue counts the drop.
func (s *errorState) report(err error) {
	if err == nil {
		return
	}

	s.mu.Lock()
	s.retainLocked(err)
	s.mu.Unlock()

	if s.notices == nil {
		return
	}
	select {
	case s.notices <- err:
	default:
		s.dropped.Add(1)
	}
}

// retainLocked keeps the first error and a sliding window of the most recent
// ones, counting what falls out of the window.
func (s *errorState) retainLocked(err error) {
	if s.first == nil {
		s.first = err
		return
	}
	if len(s.recent) < retainedErrorLimit-1 {
		s.recent = append(s.recent, err)
		return
	}
	copy(s.recent, s.recent[1:])
	s.recent[len(s.recent)-1] = err
	s.omitted++
}

// deliver runs on its own goroutine, invoking the handler for each queued
// notification and summarising drops, until close ends the queue.
func (s *errorState) deliver() {
	for err := range s.notices {
		s.invoke(err)
		s.deliverDropped()
	}
	s.deliverDropped()
}

// deliverDropped tells the handler how many notifications were dropped since
// the last summary, if any.
func (s *errorState) deliverDropped() {
	if n := s.dropped.Swap(0); n > 0 {
		s.invoke(fmt.Errorf("logrotate: %d error handler notifications omitted", n))
	}
}

// invoke calls the handler, recovering any panic so a faulty handler cannot
// kill the delivery goroutine.
func (s *errorState) invoke(err error) {
	defer func() { _ = recover() }()
	s.handler(err)
}

// close stops notification after all Writer and maintenance producers have
// exited. It deliberately does not wait for a user handler that may be slow or
// may itself be calling Shutdown.
func (s *errorState) close() {
	if s.notices != nil {
		s.closeOnce.Do(func() { close(s.notices) })
	}
}

// err assembles the journal into one error: the first, a note on how many
// were omitted, then the most recent ones; nil when nothing was reported.
func (s *errorState) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.first == nil {
		return nil
	}

	errList := make([]error, 0, 2+len(s.recent))
	errList = append(errList, s.first)
	if s.omitted > 0 {
		omitted := fmt.Errorf("logrotate: %d earlier maintenance errors omitted", s.omitted)
		errList = append(errList, omitted)
	}
	errList = append(errList, s.recent...)
	if len(errList) == 1 {
		return errList[0]
	}
	return errors.Join(errList...)
}

// reportError journals a maintenance error for Shutdown and notifies the
// error handler, if any.
func (w *Writer) reportError(err error) {
	w.errors.report(err)
}
