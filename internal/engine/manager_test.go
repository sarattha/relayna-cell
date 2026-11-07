package engine

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sarattha/relayna-cell/internal/audit"
	"github.com/sarattha/relayna-cell/internal/runtime"
	"github.com/sarattha/relayna-cell/pkg/cell"
)

type fakeBackend struct {
	mu             sync.Mutex
	removeCount    int
	prepareStarted chan struct{}
	blockPrepare   bool
}

func (f *fakeBackend) Check(context.Context) error     { return nil }
func (f *fakeBackend) Reconcile(context.Context) error { return nil }
func (f *fakeBackend) Prepare(ctx context.Context, id string, s cell.Spec) (runtime.Prepared, error) {
	if f.prepareStarted != nil {
		close(f.prepareStarted)
	}
	if f.blockPrepare {
		<-ctx.Done()
		return runtime.Prepared{}, ctx.Err()
	}
	return runtime.Prepared{ContainerID: "runtime-" + id}, nil
}
func (f *fakeBackend) Start(context.Context, string) error           { return nil }
func (f *fakeBackend) Running(context.Context, string) (bool, error) { return true, nil }
func (f *fakeBackend) Kill(context.Context, string) error            { return nil }
func (f *fakeBackend) Remove(context.Context, runtime.Prepared) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCount++
	return nil
}
func (f *fakeBackend) Wait(ctx context.Context, _ string) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}
func (f *fakeBackend) Logs(ctx context.Context, _ string, out, err io.Writer) error {
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeBackend) Exec(ctx context.Context, _ string, cmd []string, in io.Reader, out, err io.Writer) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}
func managerForTest(t *testing.T, f *fakeBackend) *Manager {
	t.Helper()
	s, e := audit.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	m, e := New(s, f)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { m.Close(); s.Close() })
	return m
}
func TestShutdownCancelsPreparation(t *testing.T) {
	f := &fakeBackend{prepareStarted: make(chan struct{}), blockPrepare: true}
	m := managerForTest(t, f)
	created := make(chan error, 1)
	go func() { _, e := m.Create(context.Background(), cell.Spec{Image: "test"}); created <- e }()
	<-f.prepareStarted
	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not cancel preparation")
	}
	if e := <-created; e == nil {
		t.Fatal("creation succeeded during shutdown")
	}
}
func TestDisconnectedObserverStopsActiveCell(t *testing.T) {
	f := &fakeBackend{}
	m := managerForTest(t, f)
	c, e := m.Create(context.Background(), cell.Spec{Image: "test"})
	if e != nil {
		t.Fatal(e)
	}
	m.ObserverDisconnected(c.ContainerID)
	m.mu.Lock()
	done := m.cells[c.ID].done
	m.mu.Unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Cell survived audit disconnect")
	}
	got, e := m.Get(c.ID)
	if e != nil || got.State != "failed" {
		t.Fatalf("state %+v %v", got, e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeCount != 1 {
		t.Fatalf("cleanup count %d", f.removeCount)
	}
}
func TestCancelledExecutionStopsWholeCell(t *testing.T) {
	f := &fakeBackend{}
	m := managerForTest(t, f)
	c, e := m.Create(context.Background(), cell.Spec{Image: "test"})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e = m.Exec(ctx, c.ID, cell.ExecRequest{Command: []string{"sleep", "10"}}, func(cell.Frame) error { return nil }, true)
	if !errors.Is(e, context.Canceled) {
		t.Fatalf("expected cancellation: %v", e)
	}
	if e = m.Delete(c.ID); e != nil {
		t.Fatal(e)
	}
	ev, e := m.Store.Events(c.ID, 0, 100)
	if e != nil || len(ev) == 0 {
		t.Fatal("audit disappeared")
	}
}
