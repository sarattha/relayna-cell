// Package cell defines the versioned public contract shared by the CLI and server.
package cell

import (
	"fmt"
	"net"
	"strings"
	"time"
)

const APIVersion = "v1alpha1"

type Endpoint struct {
	Host         string `json:"host"`
	Port         int    `json:"port"`
	AllowPrivate bool   `json:"allow_private,omitempty"`
}

type Spec struct {
	Name           string     `json:"name"`
	Image          string     `json:"image"`
	Mode           string     `json:"mode"` // session or agent
	Command        []string   `json:"command,omitempty"`
	CPU            float64    `json:"cpu"`
	MemoryMiB      int64      `json:"memory_mib"`
	WorkspaceMiB   int64      `json:"workspace_mib"`
	PIDs           int64      `json:"pids"`
	TimeoutSeconds int        `json:"timeout_seconds"`
	Allow          []Endpoint `json:"allow,omitempty"`
}

func (s *Spec) Defaults() {
	if s.Mode == "" {
		s.Mode = "session"
	}
	if s.CPU == 0 {
		s.CPU = 1
	}
	if s.MemoryMiB == 0 {
		s.MemoryMiB = 256
	}
	if s.WorkspaceMiB == 0 {
		s.WorkspaceMiB = 64
	}
	if s.PIDs == 0 {
		s.PIDs = 64
	}
	if s.TimeoutSeconds == 0 {
		s.TimeoutSeconds = 300
	}
}

func (s Spec) Validate() error {
	if s.Image == "" || len(s.Image) > 512 {
		return fmt.Errorf("image is required (max 512 bytes)")
	}
	if len(s.Name) > 128 {
		return fmt.Errorf("name exceeds 128 bytes")
	}
	if s.Mode != "session" && s.Mode != "agent" {
		return fmt.Errorf("mode must be session or agent")
	}
	if s.Mode == "agent" && len(s.Command) == 0 {
		return fmt.Errorf("agent mode requires a command")
	}
	if s.Mode == "session" && len(s.Command) > 0 {
		return fmt.Errorf("session mode uses cell-init; submit commands through exec")
	}
	if s.CPU < 0.1 || s.CPU > 4 || s.MemoryMiB < 64 || s.MemoryMiB > 2048 || s.WorkspaceMiB < 1 || s.WorkspaceMiB > s.MemoryMiB/2 || s.PIDs < 8 || s.PIDs > 256 || s.TimeoutSeconds < 1 || s.TimeoutSeconds > 3600 {
		return fmt.Errorf("resources exceed MVP limits: CPU .1–4, memory 64–2048 MiB, workspace 1–memory/2 MiB, PIDs 8–256, timeout 1–3600s")
	}
	if err := ValidateCommand(s.Command, s.Mode == "session"); err != nil {
		return err
	}
	if len(s.Allow) > 16 {
		return fmt.Errorf("maximum 16 allowed endpoints")
	}
	for _, e := range s.Allow {
		if e.Host == "" || len(e.Host) > 253 || e.Host != strings.ToLower(e.Host) || strings.HasSuffix(e.Host, ".") || strings.ContainsAny(e.Host, "/\\@*% \t\r\n") || e.Port < 1 || e.Port > 65535 {
			return fmt.Errorf("invalid exact endpoint %q:%d", e.Host, e.Port)
		}
		if net.ParseIP(e.Host) == nil {
			for _, label := range strings.Split(e.Host, ".") {
				if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
					return fmt.Errorf("invalid hostname")
				}
				for _, r := range label {
					if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
						return fmt.Errorf("invalid hostname")
					}
				}
			}
		}
	}
	return nil
}

func ValidateCommand(cmd []string, emptyOK bool) error {
	if len(cmd) == 0 && !emptyOK {
		return fmt.Errorf("command is required")
	}
	n := 0
	for _, a := range cmd {
		n += len(a)
		if strings.ContainsRune(a, 0) {
			return fmt.Errorf("command contains NUL")
		}
	}
	if len(cmd) > 256 || n > 16384 {
		return fmt.Errorf("command exceeds limits")
	}
	return nil
}

type Cell struct {
	ID          string    `json:"id"`
	Spec        Spec      `json:"spec"`
	State       string    `json:"state"`
	ContainerID string    `json:"container_id,omitempty"`
	NetworkID   string    `json:"network_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type ExecRequest struct {
	Command        []string `json:"command"`
	Stdin          []byte   `json:"stdin,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

type Frame struct {
	ExecutionID string `json:"execution_id"`
	Stream      string `json:"stream"`
	Data        []byte `json:"data,omitempty"`
	ExitCode    *int   `json:"exit_code,omitempty"`
	Error       string `json:"error,omitempty"`
}

type Event struct {
	Sequence    int64     `json:"sequence"`
	ID          string    `json:"event_id"`
	Time        time.Time `json:"timestamp"`
	CellID      string    `json:"cell_id,omitempty"`
	ExecutionID string    `json:"execution_id,omitempty"`
	Source      string    `json:"source"`
	Kind        string    `json:"event_kind"`
	Data        any       `json:"data,omitempty"`
}
