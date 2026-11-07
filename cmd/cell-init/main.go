// cell-init is a small static workload helper. It has no control-plane access.
package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) == 2 && os.Args[1] == "idle" {
		c := make(chan os.Signal, 8)
		signal.Notify(c, syscall.SIGTERM, syscall.SIGINT, syscall.SIGCHLD)
		for sig := range c {
			if sig != syscall.SIGCHLD {
				return nil
			}
			for {
				var status syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
				if err != nil || pid <= 0 {
					break
				}
			}
		}
		return nil
	}
	if len(os.Args) != 3 {
		return fmt.Errorf("usage: cell-init idle | read PATH | write PATH")
	}
	p := os.Args[2]
	if !filepath.IsLocal(p) || strings.ContainsRune(p, 0) {
		return fmt.Errorf("path must stay inside workspace")
	}
	root, err := os.OpenRoot("/workspace")
	if err != nil {
		return err
	}
	defer root.Close()
	switch os.Args[1] {
	case "write":
		if err = root.MkdirAll(filepath.Dir(p), 0755); err != nil {
			return err
		}
		// Refuse symlink targets: don't truncate an existing target through a link.
		if info, e := root.Lstat(p); e == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("target must be a regular file")
		}
		f, err := root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return err
		}
		defer f.Close()
		n, err := io.Copy(f, io.LimitReader(os.Stdin, (4<<20)+1))
		if err != nil {
			return err
		}
		if n > 4<<20 {
			return fmt.Errorf("file exceeds 4 MiB")
		}
		return f.Sync()
	case "read":
		f, err := root.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 4<<20 {
			return fmt.Errorf("file must be regular and at most 4 MiB")
		}
		_, err = io.Copy(os.Stdout, io.LimitReader(f, 4<<20))
		return err
	default:
		return fmt.Errorf("unknown operation")
	}
}
