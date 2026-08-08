package logrotate

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

const (
	retainedErrorLimit = 16
	errorNoticeBuffer  = 16
)

// errorState separates durable error ownership from best-effort notification.
// The journal is bounded and returned by Shutdown. When configured, the handler
// runs serially on its own goroutine, never under a Writer or journal lock.
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

func (s *errorState) start() {
	if s.notices != nil {
		go s.deliver()
	}
}

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

func (s *errorState) deliver() {
	for err := range s.notices {
		s.invoke(err)
		s.deliverDropped()
	}
	s.deliverDropped()
}

func (s *errorState) deliverDropped() {
	if n := s.dropped.Swap(0); n > 0 {
		s.invoke(fmt.Errorf("logrotate: %d error handler notifications omitted", n))
	}
}

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

func (s *errorState) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.first == nil {
		return nil
	}

	errList := make([]error, 0, 2+len(s.recent))
	errList = append(errList, s.first)
	if s.omitted > 0 {
		errList = append(errList,
			fmt.Errorf("logrotate: %d earlier maintenance errors omitted", s.omitted))
	}
	errList = append(errList, s.recent...)
	if len(errList) == 1 {
		return errList[0]
	}
	return errors.Join(errList...)
}

func (w *Writer) reportError(err error) {
	w.errors.report(err)
}

func newErrorState(handler func(error)) *errorState {
	s := &errorState{handler: handler}
	if handler != nil {
		s.notices = make(chan error, errorNoticeBuffer)
	}
	return s
}
