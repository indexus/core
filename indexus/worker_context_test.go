package indexus

import (
	"context"
	"testing"
	"time"
)

type cancellableJob struct{ started, stopped chan struct{} }

func (*cancellableJob) Delay() time.Duration { return time.Hour }
func (*cancellableJob) Observe() error       { return nil }
func (*cancellableJob) Refresh() error       { return nil }
func (*cancellableJob) Update() error        { panic("worker did not pass context") }
func (s *cancellableJob) UpdateContext(ctx context.Context) error {
	close(s.started)
	<-ctx.Done()
	close(s.stopped)
	return ctx.Err()
}
func TestWorkerCloseCancelsInFlightContextualUpdate(t *testing.T) {
	t.Parallel()
	s := &cancellableJob{make(chan struct{}), make(chan struct{})}
	w := NewWorker(s)
	done := make(chan struct{})
	go func() { _ = w.Start(); close(done) }()
	select {
	case <-s.started:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	w.Close()
	select {
	case <-s.stopped:
	case <-time.After(time.Second):
		t.Fatal("update did not cancel")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

type cancellableRefreshJob struct {
	cancellableJob
	refreshStarted, refreshStopped chan struct{}
}

func (s *cancellableRefreshJob) RefreshContext(ctx context.Context) error {
	close(s.refreshStarted)
	<-ctx.Done()
	close(s.refreshStopped)
	return ctx.Err()
}
func TestWorkerCloseCancelsInFlightContextualRefresh(t *testing.T) {
	t.Parallel()
	s := &cancellableRefreshJob{cancellableJob{make(chan struct{}), make(chan struct{})}, make(chan struct{}), make(chan struct{})}
	w := NewWorker(s)
	done := make(chan struct{})
	go func() { _ = w.Start(); close(done) }()
	select {
	case <-s.refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("refresh not started")
	}
	w.Close()
	select {
	case <-s.refreshStopped:
	case <-time.After(time.Second):
		t.Fatal("refresh did not cancel")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}
