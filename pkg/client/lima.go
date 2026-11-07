package client

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"
)

// NewLima carries HTTP over SSH stdio to the root-only API socket. No Docker or
// Cell management port is exposed to the Mac network or sandbox workloads.
func NewLima(vm string) (*Client, error) {
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`).MatchString(vm) {
		return nil, fmt.Errorf("invalid Lima VM name")
	}
	return &Client{HTTP: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cmd := exec.Command("limactl", "shell", vm, "sudo", "/opt/relayna-cell/bin/cell", "proxy")
		cmd.Stderr = os.Stderr
		in, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			in.Close()
			return nil, err
		}
		if err = cmd.Start(); err != nil {
			in.Close()
			out.Close()
			return nil, err
		}
		p := &pipeConn{in: in, out: out, cmd: cmd, done: make(chan struct{})}
		go func() { cmd.Wait(); close(p.done) }()
		return p, nil
	}}}}, nil
}

type pipeConn struct {
	in   io.WriteCloser
	out  io.ReadCloser
	cmd  *exec.Cmd
	once sync.Once
	done chan struct{}
}

func (p *pipeConn) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *pipeConn) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *pipeConn) Close() error {
	p.once.Do(func() { p.in.Close(); p.out.Close(); p.cmd.Process.Kill(); <-p.done })
	return nil
}

type pipeAddr string

func (a pipeAddr) Network() string       { return "ssh-stdio" }
func (a pipeAddr) String() string        { return string(a) }
func (p *pipeConn) LocalAddr() net.Addr  { return pipeAddr("mac") }
func (p *pipeConn) RemoteAddr() net.Addr { return pipeAddr("cell-vm") }
func (p *pipeConn) SetDeadline(t time.Time) error {
	if e := p.SetReadDeadline(t); e != nil {
		return e
	}
	return p.SetWriteDeadline(t)
}
func (p *pipeConn) SetReadDeadline(t time.Time) error  { return p.out.(*os.File).SetReadDeadline(t) }
func (p *pipeConn) SetWriteDeadline(t time.Time) error { return p.in.(*os.File).SetWriteDeadline(t) }
