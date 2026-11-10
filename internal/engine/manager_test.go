package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
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
func managerForTest(t *testing.T, f Backend) *Manager {
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

type logStreamBackend struct {
	fakeBackend
	logs    func(context.Context) error
	wait    func(context.Context) (int, error)
	running bool
	kills   int
}

func (f *logStreamBackend) Logs(ctx context.Context, _ string, _, _ io.Writer) error {
	return f.logs(ctx)
}
func (f *logStreamBackend) Wait(ctx context.Context, _ string) (int, error) {
	return f.wait(ctx)
}
func (f *logStreamBackend) Running(context.Context, string) (bool, error) {
	return f.running, nil
}
func (f *logStreamBackend) Kill(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kills++
	return nil
}

func TestLogStreamFailureStopsRunningCell(t *testing.T) {
	for _, logErr := range []error{errors.New("log stream disconnected"), nil} {
		name := "error"
		if logErr == nil {
			name = "unexpected EOF"
		}
		t.Run(name, func(t *testing.T) {
			failLogs := make(chan struct{})
			f := &logStreamBackend{
				running: true,
				logs:    func(context.Context) error { <-failLogs; return logErr },
				wait:    func(ctx context.Context) (int, error) { <-ctx.Done(); return 0, ctx.Err() },
			}
			m := managerForTest(t, f)
			c, err := m.Create(context.Background(), cell.Spec{Image: "test", TimeoutSeconds: 3600})
			if err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			done := m.cells[c.ID].done
			m.mu.Unlock()
			close(failLogs)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("workload survived output collection failure")
			}
			got, err := m.Get(c.ID)
			if err != nil || got.State != "failed" || !strings.Contains(got.Error, "output collection") {
				t.Fatalf("state %+v, error %v", got, err)
			}
			f.mu.Lock()
			kills, removes := f.kills, f.removeCount
			f.mu.Unlock()
			if kills == 0 || removes != 1 {
				t.Fatalf("kills %d, cleanup count %d", kills, removes)
			}
			events, err := m.Store.Events(c.ID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Kind == "audit.gap" {
					return
				}
			}
			t.Fatal("missing audit.gap event")
		})
	}
}

func TestNormalExitDrainsLogsWithoutAuditFault(t *testing.T) {
	for _, logsFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("logsFirst=%v", logsFirst), func(t *testing.T) {
			finish := make(chan struct{})
			f := &logStreamBackend{
				logs: func(context.Context) error {
					if !logsFirst {
						<-finish
					}
					return nil
				},
				wait: func(context.Context) (int, error) {
					if logsFirst {
						<-finish
					}
					return 7, nil
				},
			}
			m := managerForTest(t, f)
			c, err := m.Create(context.Background(), cell.Spec{Image: "test"})
			if err != nil {
				t.Fatal(err)
			}
			close(finish)
			m.mu.Lock()
			done := m.cells[c.ID].done
			m.mu.Unlock()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("normal exit did not finish")
			}
			got, err := m.Get(c.ID)
			if err != nil || got.State != "exited" || got.Error != "" || got.ExitCode == nil || *got.ExitCode != 7 {
				t.Fatalf("state %+v, error %v", got, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.kills != 0 || f.removeCount != 1 {
				t.Fatalf("kills %d, cleanup count %d", f.kills, f.removeCount)
			}
		})
	}
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
	e = m.Exec(ctx, c.ID, cell.ExecRequest{Command: []string{"sleep", "10"}}, func(context.Context, cell.Frame) error { return nil }, true)
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

type completedExecBackend struct{ fakeBackend }

func (*completedExecBackend) Exec(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) {
	return 1, nil
}

func TestExitFrameErrorStopsStreamedExecButKeepsFileHelperSession(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			m := managerForTest(t, &completedExecBackend{})
			c, err := m.Create(context.Background(), cell.Spec{Image: "test"})
			if err != nil {
				t.Fatal(err)
			}
			callbackErr := errors.New("exit callback failed")
			err = m.Exec(context.Background(), c.ID, cell.ExecRequest{Command: []string{"helper"}}, func(ctx context.Context, frame cell.Frame) error {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("exit callback has no execution deadline")
				}
				return callbackErr
			}, stream)
			if !errors.Is(err, callbackErr) {
				t.Fatalf("error %v", err)
			}
			got, err := m.Get(c.ID)
			want := "running"
			if stream {
				want = "failed"
			}
			if err != nil || got.State != want {
				t.Fatalf("state %+v, error %v", got, err)
			}
		})
	}
}
