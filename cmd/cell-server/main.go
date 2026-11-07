package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sarattha/relayna-cell/internal/api"
	"github.com/sarattha/relayna-cell/internal/audit"
	"github.com/sarattha/relayna-cell/internal/engine"
	"github.com/sarattha/relayna-cell/internal/observer"
	"github.com/sarattha/relayna-cell/internal/runtime"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	data := flag.String("data-dir", "/var/lib/relayna-cell", "persistent state directory")
	rundir := flag.String("run-dir", "/run/relayna-cell", "Unix socket directory")
	flag.Parse()
	if os.Geteuid() != 0 {
		return fmt.Errorf("run cell-server as root inside the dedicated Linux VM")
	}
	if e := os.MkdirAll(*rundir, 0700); e != nil {
		return e
	}
	// Exclusive lifetime lock protects the shared journal, sockets, and reconciliation.
	lock, e := os.OpenFile(filepath.Join(*rundir, "server.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return fmt.Errorf("another server is running: %w", e)
	}
	store, e := audit.Open(*data)
	if e != nil {
		return e
	}
	defer store.Close()
	backend, e := runtime.New()
	if e != nil {
		return e
	}
	defer backend.Client.Close()
	m, e := engine.New(store, backend)
	if e != nil {
		return e
	}
	defer m.Close()
	for _, name := range []string{"observer.sock", "api.sock"} {
		if e = os.Remove(filepath.Join(*rundir, name)); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	o, e := observer.Start(filepath.Join(*rundir, "observer.sock"), m.ObserverEvent, m.Fault, m.ObserverDisconnected)
	if e != nil {
		return e
	}
	defer o.Close()
	l, e := net.Listen("unix", filepath.Join(*rundir, "api.sock"))
	if e != nil {
		return e
	}
	defer l.Close()
	if e = os.Chmod(filepath.Join(*rundir, "api.sock"), 0600); e != nil {
		return e
	}
	proxyListener, e := net.Listen("tcp4", ":3128")
	if e != nil {
		return e
	}
	defer proxyListener.Close()
	srv := &http.Server{Handler: api.Handler(m), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	proxy := &http.Server{Handler: m.Proxy, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	errs := make(chan error, 2)
	go func() { errs <- srv.Serve(l) }()
	go func() { errs <- proxy.Serve(proxyListener) }()
	if e = store.Emit("", "", "control", "server.started", map[string]string{"runtime": runtime.RuntimeName}); e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case e = <-errs:
	}
	m.Close()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
	proxy.Shutdown(shutdown)
	o.Close()
	return e
}
