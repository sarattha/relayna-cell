package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"time"

	"github.com/sarattha/relayna-cell/pkg/cell"
	"github.com/sarattha/relayna-cell/pkg/client"
)

func main() {
	code, e := run()
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	os.Exit(code)
}
func run() (int, error) {
	global := flag.NewFlagSet("cell", flag.ContinueOnError)
	socket := global.String("socket", "/run/relayna-cell/api.sock", "server socket")
	vm := global.String("lima", "", "connect to a Lima VM over SSH stdio")
	if e := global.Parse(os.Args[1:]); e != nil {
		return 1, e
	}
	args := global.Args()
	if len(args) == 0 {
		return 1, fmt.Errorf("usage: cell [--socket PATH] create|list|inspect|exec|put|get|events|delete|health")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	c := client.New(*socket)
	if *vm != "" {
		var e error
		c, e = client.NewLima(*vm)
		if e != nil {
			return 1, e
		}
	}
	defer c.HTTP.CloseIdleConnections()
	copyResponse := func(method, path string, body io.Reader) error {
		r, e := c.Request(ctx, method, path, body)
		if e != nil {
			return e
		}
		defer r.Body.Close()
		_, e = io.Copy(os.Stdout, r.Body)
		return e
	}
	switch args[0] {
	case "proxy":
		conn, e := net.Dial("unix", *socket)
		if e != nil {
			return 1, e
		}
		defer conn.Close()
		go func() { io.Copy(conn, os.Stdin); conn.(*net.UnixConn).CloseWrite() }()
		_, e = io.Copy(os.Stdout, conn)
		return 0, e
	case "health":
		return 0, copyResponse("GET", "/healthz", nil)
	case "list":
		return 0, copyResponse("GET", "/v1alpha1/cells", nil)
	case "create":
		f := flag.NewFlagSet("create", flag.ContinueOnError)
		spec := f.String("f", "", "JSON Cell spec")
		if e := f.Parse(args[1:]); e != nil {
			return 1, e
		}
		if *spec == "" {
			return 1, fmt.Errorf("create requires -f SPEC.json")
		}
		b, e := os.Open(*spec)
		if e != nil {
			return 1, e
		}
		defer b.Close()
		return 0, copyResponse("POST", "/v1alpha1/cells", b)
	case "inspect", "delete":
		if len(args) != 2 {
			return 1, fmt.Errorf("%s requires Cell ID", args[0])
		}
		method := "GET"
		if args[0] == "delete" {
			method = "DELETE"
		}
		return 0, copyResponse(method, "/v1alpha1/cells/"+url.PathEscape(args[1]), nil)
	case "exec":
		f := flag.NewFlagSet("exec", flag.ContinueOnError)
		timeout := f.Int("timeout", 60, "seconds; expiration stops the Cell")
		if e := f.Parse(args[1:]); e != nil {
			return 1, e
		}
		a := f.Args()
		if len(a) < 2 {
			return 1, fmt.Errorf("exec [--timeout N] CELL_ID -- COMMAND [ARGS]")
		}
		cmd := a[1:]
		if cmd[0] == "--" {
			cmd = cmd[1:]
		}
		code := 0
		e := c.Exec(ctx, a[0], cell.ExecRequest{Command: cmd, TimeoutSeconds: *timeout}, func(frame cell.Frame) error {
			var e error
			if frame.Stream == "stdout" {
				_, e = os.Stdout.Write(frame.Data)
			}
			if frame.Stream == "stderr" {
				_, e = os.Stderr.Write(frame.Data)
			}
			if frame.ExitCode != nil {
				code = *frame.ExitCode
			}
			return e
		})
		return code, e
	case "put":
		if len(args) != 4 {
			return 1, fmt.Errorf("put CELL_ID LOCAL_FILE WORKSPACE_PATH")
		}
		f, e := os.Open(args[2])
		if e != nil {
			return 1, e
		}
		defer f.Close()
		return 0, copyResponse("PUT", "/v1alpha1/cells/"+url.PathEscape(args[1])+"/files?path="+url.QueryEscape(args[3]), f)
	case "get":
		if len(args) != 3 {
			return 1, fmt.Errorf("get CELL_ID WORKSPACE_PATH (writes to stdout)")
		}
		return 0, copyResponse("GET", "/v1alpha1/cells/"+url.PathEscape(args[1])+"/files?path="+url.QueryEscape(args[2]), nil)
	case "events":
		f := flag.NewFlagSet("events", flag.ContinueOnError)
		follow := f.Bool("follow", false, "follow events")
		after := f.Int64("after", 0, "sequence cursor")
		if e := f.Parse(args[1:]); e != nil {
			return 1, e
		}
		id := ""
		if len(f.Args()) > 0 {
			id = f.Args()[0]
		}
		enc := json.NewEncoder(os.Stdout)
		for {
			events, e := c.Events(ctx, id, *after)
			if e != nil {
				return 1, e
			}
			for _, event := range events {
				if e = enc.Encode(event); e != nil {
					return 1, e
				}
				*after = event.Sequence
			}
			if len(events) == 1000 {
				continue
			}
			if !*follow {
				return 0, nil
			}
			select {
			case <-ctx.Done():
				return 0, nil
			case <-time.After(time.Second):
			}
		}
	default:
		return 1, fmt.Errorf("unknown command %q", args[0])
	}
}
