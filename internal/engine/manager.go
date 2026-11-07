package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/sarattha/relayna-cell/internal/audit"
	"github.com/sarattha/relayna-cell/internal/network"
	"github.com/sarattha/relayna-cell/internal/runtime"
	"github.com/sarattha/relayna-cell/pkg/cell"
)

type Backend interface {
	Check(context.Context) error
	Prepare(context.Context, string, cell.Spec) (runtime.Prepared, error)
	Start(context.Context, string) error
	Kill(context.Context, string) error
	Remove(context.Context, runtime.Prepared) error
	Wait(context.Context, string) (int, error)
	Logs(context.Context, string, io.Writer, io.Writer) error
	Exec(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error)
	Reconcile(context.Context) error
	Running(context.Context, string) (bool, error)
}
type instance struct {
	id       string
	cell     cell.Cell
	prepared runtime.Prepared
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	busy     bool
}
type Manager struct {
	mu         sync.Mutex
	cells      map[string]*instance
	containers map[string]string
	Store      *audit.Store
	Backend    Backend
	Proxy      *network.Proxy
	wg         sync.WaitGroup
	closing    bool
}

func New(store *audit.Store, backend Backend) (*Manager, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := backend.Check(ctx); err != nil {
		return nil, err
	}
	if err := backend.Reconcile(ctx); err != nil {
		return nil, err
	}
	m := &Manager{cells: map[string]*instance{}, containers: map[string]string{}, Store: store, Backend: backend}
	old, err := store.Cells()
	if err != nil {
		return nil, err
	}
	for _, c := range old {
		if c.State == "running" || c.State == "preparing" {
			c.State = "interrupted"
			c.Error = "server restarted; runtime removed to avoid unaudited continuation"
			if err = store.Save(c); err != nil {
				return nil, err
			}
			if err = store.Emit(c.ID, "", "control", "cell.interrupted", nil); err != nil {
				return nil, err
			}
		}
		done := make(chan struct{})
		close(done)
		m.cells[c.ID] = &instance{id: c.ID, cell: c, done: done}
	}
	m.Proxy = network.New(func(id, kind string, data any) error { return m.emit(id, "", "network", kind, data) })
	return m, nil
}
func (m *Manager) emit(id, execID, source, kind string, data any) error {
	err := m.Store.Emit(id, execID, source, kind, data)
	if err != nil {
		go m.Fault("", err)
	}
	return err
}
func (m *Manager) ObserverEvent(containerID, kind string, data any) error {
	m.mu.Lock()
	id := m.containers[containerID]
	m.mu.Unlock()
	if id == "" {
		return fmt.Errorf("unregistered runtime identity %q", containerID)
	}
	return m.emit(id, "", "runtime", kind, data)
}

func (m *Manager) ObserverDisconnected(containerID string) {
	m.mu.Lock()
	id := m.containers[containerID]
	i := m.cells[id]
	active := !m.closing && i != nil && (i.cell.State == "running" || i.cell.State == "preparing")
	m.mu.Unlock()
	if !active {
		return
	}
	// The trace socket may close just before Docker reports normal process exit.
	select {
	case <-i.ctx.Done():
		return
	case <-time.After(250 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	running, err := m.Backend.Running(ctx, containerID)
	if err != nil || running {
		m.Fault(containerID, fmt.Errorf("runtime audit connection closed while Cell was active"))
	}
}
func (m *Manager) Fault(containerID string, err error) {
	m.mu.Lock()
	var targets []*instance
	for _, i := range m.cells {
		if (i.cell.State == "running" || i.cell.State == "preparing") && (containerID == "" || i.cell.ContainerID == containerID) {
			i.cell.State = "failed"
			i.cell.Error = "audit failure: " + err.Error()
			targets = append(targets, i)
			m.Store.Save(i.cell)
		}
	}
	m.mu.Unlock()
	for _, i := range targets {
		_ = m.Store.Emit(i.id, "", "control", "audit.gap", map[string]string{"error": err.Error()})
		m.stop(i)
	}
}
func (m *Manager) Create(ctx context.Context, s cell.Spec) (cell.Cell, error) {
	s.Defaults()
	if err := s.Validate(); err != nil {
		return cell.Cell{}, err
	}
	if err := m.Store.Healthy(); err != nil {
		return cell.Cell{}, err
	}
	m.mu.Lock()
	active := 0
	for _, i := range m.cells {
		if i.cell.State == "running" || i.cell.State == "preparing" {
			active++
		}
	}
	if m.closing || active >= 8 {
		m.mu.Unlock()
		return cell.Cell{}, fmt.Errorf("runtime unavailable or maximum 8 active Cells reached")
	}
	now := time.Now().UTC()
	id := audit.ID()
	i := &instance{id: id, cell: cell.Cell{ID: id, Spec: s, State: "preparing", CreatedAt: now, ExpiresAt: now.Add(time.Duration(s.TimeoutSeconds) * time.Second)}, done: make(chan struct{})}
	i.ctx, i.cancel = context.WithDeadline(context.Background(), i.cell.ExpiresAt)
	m.cells[id] = i
	m.wg.Add(1)
	initial := i.cell
	m.mu.Unlock()
	defer m.wg.Done()
	fail := func(err error) (cell.Cell, error) {
		i.cancel()
		m.mu.Lock()
		i.cell.State = "failed"
		i.cell.Error = err.Error()
		m.Store.Save(i.cell)
		c := i.cell
		m.mu.Unlock()
		close(i.done)
		return c, err
	}
	if err := m.Store.Save(initial); err != nil {
		return fail(err)
	}
	if err := m.emit(i.id, "", "control", "cell.requested", s); err != nil {
		return fail(err)
	}
	prepCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stopPrepare := context.AfterFunc(i.ctx, cancel)
	defer stopPrepare()
	p, err := m.Backend.Prepare(prepCtx, i.id, s)
	if err != nil {
		return fail(err)
	}
	m.mu.Lock()
	i.prepared = p
	i.cell.ContainerID = p.ContainerID
	i.cell.NetworkID = p.NetworkID
	m.containers[p.ContainerID] = i.id
	failed := i.cell.State == "failed" || m.closing
	m.mu.Unlock()
	cleanup := func() {
		i.cancel()
		m.Proxy.Revoke(i.id)
		c, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		m.Backend.Remove(c, p)
	}
	if failed {
		cleanup()
		return fail(fmt.Errorf("startup cancelled"))
	}
	if p.IP != "" {
		m.Proxy.Register(p.IP, network.Grant{CellID: i.id, Allow: s.Allow, Context: i.ctx})
	}
	if err = m.emit(i.id, "", "control", "cell.prepared", map[string]any{"container_id": p.ContainerID, "expires_at": i.cell.ExpiresAt}); err != nil {
		cleanup()
		return fail(err)
	}
	if err = m.Backend.Start(prepCtx, p.ContainerID); err != nil {
		cleanup()
		return fail(err)
	}
	m.mu.Lock()
	if i.cell.State != "failed" {
		i.cell.State = "running"
	}
	c := i.cell
	err = m.Store.Save(c)
	m.wg.Add(1)
	m.mu.Unlock()
	go m.watch(i)
	if err != nil {
		m.Fault(p.ContainerID, err)
		return c, err
	}
	return c, nil
}
func (m *Manager) watch(i *instance) {
	defer m.wg.Done()
	defer close(i.done)
	logsDone := make(chan error, 1)
	budget := &outputBudget{max: 4 << 20}
	go func() {
		logsDone <- m.Backend.Logs(i.ctx, i.prepared.ContainerID, m.writer(i, "", "stdout", budget, nil, true), m.writer(i, "", "stderr", budget, nil, true))
	}()
	code, err := m.Backend.Wait(i.ctx, i.prepared.ContainerID)
	if err != nil {
		m.stop(i)
	}
	select {
	case logErr := <-logsDone:
		if logErr != nil && err == nil {
			err = logErr
		}
	case <-time.After(2 * time.Second):
		i.cancel()
		<-logsDone
	}
	m.Proxy.Revoke(i.id)
	i.cancel()
	m.mu.Lock()
	if i.cell.State == "running" {
		if err != nil {
			i.cell.State = "failed"
			i.cell.Error = err.Error()
		} else {
			i.cell.State = "exited"
		}
	}
	i.cell.ExitCode = &code
	c := i.cell
	m.mu.Unlock()
	_ = m.emit(c.ID, "", "control", "cell.finished", map[string]any{"state": c.State, "exit_code": code, "error": c.Error})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if e := m.Backend.Remove(ctx, i.prepared); e != nil {
		m.mu.Lock()
		i.cell.Error = "cleanup failed: " + e.Error()
		i.cell.State = "failed"
		c = i.cell
		m.mu.Unlock()
		_ = m.emit(c.ID, "", "control", "cell.cleanup_failed", map[string]string{"error": e.Error()})
	}
	if e := m.Store.Save(c); e != nil {
		m.Fault("", e)
	}
}
func (m *Manager) stop(i *instance) {
	m.mu.Lock()
	cancel := i.cancel
	id := i.cell.ContainerID
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if id != "" {
		ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = m.Backend.Kill(ctx, id)
	}
}
func (m *Manager) Get(id string) (cell.Cell, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.cells[id]
	if i == nil {
		return cell.Cell{}, fmt.Errorf("Cell not found")
	}
	return i.cell, nil
}
func (m *Manager) List() ([]cell.Cell, error) { return m.Store.Cells() }
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	i := m.cells[id]
	if i == nil {
		m.mu.Unlock()
		return fmt.Errorf("Cell not found")
	}
	if i.cell.State == "preparing" {
		m.mu.Unlock()
		return fmt.Errorf("Cell is still preparing")
	}
	i.cell.State = "deleted"
	m.mu.Unlock()
	m.stop(i)
	<-i.done
	m.mu.Lock()
	c := i.cell
	c.State = "deleted"
	i.cell = c
	m.mu.Unlock()
	if err := m.Store.Save(c); err != nil {
		return err
	}
	return m.emit(id, "", "control", "cell.deleted", nil)
}

func (m *Manager) Exec(ctx context.Context, id string, req cell.ExecRequest, send func(cell.Frame) error, recordOutput bool) error {
	if err := cell.ValidateCommand(req.Command, false); err != nil {
		return err
	}
	if len(req.Stdin) > 4<<20 {
		return fmt.Errorf("stdin exceeds 4 MiB")
	}
	if req.TimeoutSeconds == 0 {
		req.TimeoutSeconds = 60
	}
	if req.TimeoutSeconds < 1 || req.TimeoutSeconds > 300 {
		return fmt.Errorf("execution timeout must be 1–300 seconds")
	}
	m.mu.Lock()
	i := m.cells[id]
	if m.closing || i == nil || i.cell.State != "running" || i.busy {
		m.mu.Unlock()
		return fmt.Errorf("Cell must be running and have no active API execution")
	}
	i.busy = true
	m.wg.Add(1)
	cid := i.cell.ContainerID
	m.mu.Unlock()
	defer m.wg.Done()
	defer func() { m.mu.Lock(); i.busy = false; m.mu.Unlock() }()
	execID := audit.ID()
	if err := m.emit(id, execID, "control", "execution.requested", map[string]any{"command": req.Command, "stdin_bytes": len(req.Stdin)}); err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds)*time.Second)
	defer cancel()
	stop := context.AfterFunc(i.ctx, cancel)
	defer stop()
	budget := &outputBudget{max: 4 << 20}
	code, err := m.Backend.Exec(runCtx, cid, req.Command, bytes.NewReader(req.Stdin), m.writer(i, execID, "stdout", budget, send, recordOutput), m.writer(i, execID, "stderr", budget, send, recordOutput))
	if err != nil || runCtx.Err() != nil {
		if err == nil {
			err = runCtx.Err()
		}
		m.mu.Lock()
		i.cell.State = "failed"
		i.cell.Error = "execution cancelled or output failed: " + err.Error()
		m.mu.Unlock()
		m.stop(i)
		_ = m.emit(id, execID, "control", "execution.failed", map[string]string{"error": err.Error()})
		return err
	}
	if err = m.emit(id, execID, "control", "execution.finished", map[string]int{"exit_code": code}); err != nil {
		return err
	}
	return send(cell.Frame{ExecutionID: execID, Stream: "exit", ExitCode: &code})
}

type outputBudget struct {
	mu     sync.Mutex
	n, max int
}
type outputWriter struct{ write func([]byte) (int, error) }

func (w outputWriter) Write(p []byte) (int, error) { return w.write(p) }
func (m *Manager) writer(i *instance, execID, stream string, b *outputBudget, send func(cell.Frame) error, record bool) io.Writer {
	return outputWriter{write: func(p []byte) (int, error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.n+len(p) > b.max {
			go m.stop(i)
			return 0, fmt.Errorf("output exceeds 4 MiB")
		}
		b.n += len(p)
		total := len(p)
		for len(p) > 0 {
			n := len(p)
			if n > 8192 {
				n = 8192
			}
			part := p[:n]
			if record {
				if err := m.emit(i.id, execID, "workload", "output", map[string]string{"stream": stream, "text": string(part)}); err != nil {
					return 0, err
				}
			}
			if send != nil {
				if err := send(cell.Frame{ExecutionID: execID, Stream: stream, Data: part}); err != nil {
					return 0, err
				}
			}
			p = p[n:]
		}
		return total, nil
	}}
}
func (m *Manager) Close() {
	m.mu.Lock()
	m.closing = true
	var all []*instance
	for _, i := range m.cells {
		if i.cell.State == "running" || i.cell.State == "preparing" {
			i.cell.State = "interrupted"
			i.cell.Error = "server shutdown"
			all = append(all, i)
		}
	}
	m.mu.Unlock()
	for _, i := range all {
		m.stop(i)
	}
	m.wg.Wait()
}
