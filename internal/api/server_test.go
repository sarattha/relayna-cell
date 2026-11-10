package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sarattha/relayna-cell/internal/audit"
	"github.com/sarattha/relayna-cell/internal/engine"
	"github.com/sarattha/relayna-cell/internal/runtime"
	"github.com/sarattha/relayna-cell/pkg/cell"
)

type streamingBackend struct {
	started  chan struct{}
	killed   chan struct{}
	once     sync.Once
	exitOnly bool
}

func (*streamingBackend) Check(context.Context) error     { return nil }
func (*streamingBackend) Reconcile(context.Context) error { return nil }
func (*streamingBackend) Prepare(_ context.Context, id string, _ cell.Spec) (runtime.Prepared, error) {
	return runtime.Prepared{ContainerID: id}, nil
}
func (*streamingBackend) Start(context.Context, string) error            { return nil }
func (*streamingBackend) Remove(context.Context, runtime.Prepared) error { return nil }
func (*streamingBackend) Running(context.Context, string) (bool, error)  { return true, nil }
func (f *streamingBackend) Kill(context.Context, string) error {
	f.once.Do(func() { close(f.killed) })
	return nil
}
func (*streamingBackend) Wait(ctx context.Context, _ string) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}
func (*streamingBackend) Logs(ctx context.Context, _ string, _, _ io.Writer) error {
	<-ctx.Done()
	return ctx.Err()
}
func (f *streamingBackend) Exec(_ context.Context, _ string, _ []string, _ io.Reader, out, _ io.Writer) (int, error) {
	close(f.started)
	if f.exitOnly {
		return 0, nil
	}
	// Synchronous output deliberately ignores cancellation, like a backend blocked
	// in the API's response writer. The client below never reads the response.
	_, err := out.Write(bytes.Repeat([]byte("x"), 32<<10))
	return 0, err
}

type pipeListener struct {
	conn     net.Conn
	accepted bool
	closed   chan struct{}
	once     sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}
func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}
func (l *pipeListener) Addr() net.Addr { return l.conn.LocalAddr() }

type observedWriter struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *observedWriter) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(p)
}

func TestUnreadExecResponseStopsCellAndAllowsShutdown(t *testing.T) {
	for _, test := range []struct{ shutdown, exitOnly bool }{{}, {shutdown: true}, {exitOnly: true}, {shutdown: true, exitOnly: true}} {
		t.Run(fmt.Sprintf("shutdown=%v/exitOnly=%v", test.shutdown, test.exitOnly), func(t *testing.T) {
			store, err := audit.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			backend := &streamingBackend{started: make(chan struct{}), killed: make(chan struct{}), exitOnly: test.exitOnly}
			m, err := engine.New(store, backend)
			if err != nil {
				store.Close()
				t.Fatal(err)
			}
			serverConn, clientConn := net.Pipe()
			writing := make(chan struct{})
			listener := &pipeListener{conn: &observedWriter{Conn: serverConn, started: writing}, closed: make(chan struct{})}
			handlerDone := make(chan struct{})
			handler := Handler(m)
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(handlerDone)
				handler.ServeHTTP(w, r)
			})}
			t.Cleanup(func() {
				clientConn.Close()
				server.Close()
				m.Close()
				store.Close()
			})
			c, err := m.Create(context.Background(), cell.Spec{Image: "test"})
			if err != nil {
				t.Fatal(err)
			}
			go server.Serve(listener)
			timeout := 1
			if test.shutdown {
				timeout = 300
			}
			body := fmt.Sprintf(`{"command":["output"],"timeout_seconds":%d}`, timeout)
			clientConn.SetWriteDeadline(time.Now().Add(3 * time.Second))
			_, err = fmt.Fprintf(clientConn, "POST /v1alpha1/cells/%s/exec HTTP/1.1\r\nHost: cell\r\nContent-Length: %d\r\n\r\n%s", c.ID, len(body), body)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-backend.started:
			case <-time.After(3 * time.Second):
				t.Fatal("execution did not start")
			}
			select {
			case <-writing:
			case <-time.After(3 * time.Second):
				t.Fatal("HTTP output did not reach the blocked connection")
			}
			closed := make(chan struct{})
			if test.shutdown {
				go func() { m.Close(); close(closed) }()
			}
			select {
			case <-backend.killed:
			case <-time.After(3 * time.Second):
				t.Fatal("Cell survived blocked output beyond cancellation")
			}
			select {
			case <-handlerDone:
			case <-time.After(3 * time.Second):
				t.Fatal("HTTP handler stayed blocked while client was not reading")
			}
			if !test.shutdown {
				go func() { m.Close(); close(closed) }()
			}
			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				t.Fatal("manager shutdown stayed blocked")
			}
			events, err := store.Events(c.ID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Kind == "execution.failed" {
					return
				}
			}
			t.Fatal("missing execution.failed event")
		})
	}
}
