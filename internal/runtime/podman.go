package runtime

// Podman uses its local, rootful CLI. It deliberately does not route through
// the Docker compatibility API, which has different tmpfs and DNS defaults.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sarattha/relayna-cell/pkg/cell"
)

const PodmanRuntime = "/opt/relayna-cell/runtime/runsc-cell"

type Podman struct{}

// Reserve/recreate Netavark's automatically chosen subnet as one operation
// within this server. An external allocation collision fails creation closed.
var podmanNetworkMu sync.Mutex

func podmanCommand(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "podman", append([]string{"--remote=false"}, args...)...)
}

func podmanOutput(ctx context.Context, args ...string) ([]byte, error) {
	cmd := podmanCommand(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("podman %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return b, nil
}

func (p *Podman) Check(ctx context.Context) error {
	b, err := podmanOutput(ctx, "info", "--format=json")
	if err != nil {
		return err
	}
	var info struct {
		Host struct {
			NetworkBackend string
			CgroupVersion  string
			Security       struct{ Rootless bool }
		}
	}
	if err = json.Unmarshal(b, &info); err != nil {
		return err
	}
	if info.Host.Security.Rootless || info.Host.NetworkBackend != "netavark" || info.Host.CgroupVersion != "v2" {
		return fmt.Errorf("rootful Podman with Netavark and cgroup v2 is required")
	}
	if b, err = exec.CommandContext(ctx, PodmanRuntime, "--version").CombinedOutput(); err != nil {
		return fmt.Errorf("required gVisor runtime: %w: %s", err, b)
	}
	if b, err = exec.CommandContext(ctx, "/bin/bash", "/opt/relayna-cell/scripts/network-guard.sh", "--engine", "podman", "--check").CombinedOutput(); err != nil {
		return fmt.Errorf("Podman network guard: %w: %s", err, b)
	}
	return nil
}

func (p *Podman) Prepare(ctx context.Context, id string, s cell.Spec) (prepared Prepared, err error) {
	if err = p.Check(ctx); err != nil {
		return
	}
	image, err := podmanOutput(ctx, "image", "inspect", "--", s.Image)
	if err != nil {
		ref := s.Image
		// Avoid interactive short-name resolution. Local images are inspected first.
		first, _, qualified := strings.Cut(ref, "/")
		if !qualified {
			ref = "docker.io/library/" + ref
		} else if !strings.ContainsAny(first, ".:") && first != "localhost" {
			ref = "docker.io/" + ref
		}
		if _, err = podmanOutput(ctx, "pull", "--quiet", "--", ref); err != nil {
			return
		}
		image, err = podmanOutput(ctx, "image", "inspect", "--", ref)
		if err != nil {
			return
		}
	}
	var images []struct {
		ID     string
		Config struct{ Volumes map[string]any }
	}
	if err = json.Unmarshal(image, &images); err != nil {
		return
	}
	if len(images) != 1 || images[0].ID == "" || len(images[0].Config.Volumes) != 0 {
		err = fmt.Errorf("invalid image or image-declared volumes are unsupported")
		return
	}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = p.Remove(cleanup, prepared)
		}
	}()
	network := "none"
	env := []string{"HOME=/workspace", "TMPDIR=/tmp"}
	if len(s.Allow) > 0 {
		podmanNetworkMu.Lock()
		defer podmanNetworkMu.Unlock()
		name, bridge := "cell-"+id, "rc"+id[:10]
		if _, err = podmanOutput(ctx, "network", "create", "--driver=bridge", "--internal", "--disable-dns",
			"--interface-name="+bridge, "--label="+Label+"="+id, name); err != nil {
			return
		}
		prepared.NetworkID = name
		var data []struct {
			NetworkInterface string `json:"network_interface"`
			Internal         bool
			DNSEnabled       bool `json:"dns_enabled"`
			IPv6Enabled      bool `json:"ipv6_enabled"`
			Subnets          []struct{ Subnet, Gateway string }
		}
		var b []byte
		b, err = podmanOutput(ctx, "network", "inspect", name)
		if err != nil {
			return
		}
		if err = json.Unmarshal(b, &data); err != nil {
			return
		}
		if len(data) != 1 || data[0].NetworkInterface != bridge || !data[0].Internal || data[0].DNSEnabled || data[0].IPv6Enabled || len(data[0].Subnets) != 1 {
			err = fmt.Errorf("Podman network does not match the isolated Cell profile")
			return
		}
		// Netavark intentionally omits a gateway on auto-subnet internal
		// networks. Supply it explicitly so the only reachable host service
		// (our guarded proxy) has an address. Keep the network internal.
		if data[0].Subnets[0].Gateway == "" {
			prefix, e := netip.ParsePrefix(data[0].Subnets[0].Subnet)
			if e != nil || !prefix.Addr().Is4() || prefix.Bits() > 30 {
				err = fmt.Errorf("invalid Podman internal subnet")
				return
			}
			if _, err = podmanOutput(ctx, "network", "rm", name); err != nil {
				return
			}
			if _, err = podmanOutput(ctx, "network", "create", "--driver=bridge", "--internal", "--disable-dns",
				"--interface-name="+bridge, "--label="+Label+"="+id, "--subnet="+prefix.String(),
				"--gateway="+prefix.Masked().Addr().Next().String(), name); err != nil {
				return
			}
			b, err = podmanOutput(ctx, "network", "inspect", name)
			if err != nil {
				return
			}
			data = nil
			if err = json.Unmarshal(b, &data); err != nil {
				return
			}
			if len(data) != 1 || data[0].NetworkInterface != bridge || !data[0].Internal || data[0].DNSEnabled || data[0].IPv6Enabled || len(data[0].Subnets) != 1 {
				err = fmt.Errorf("Podman gateway network does not match the isolated Cell profile")
				return
			}
		}
		gateway, e := netip.ParseAddr(data[0].Subnets[0].Gateway)
		subnet, se := netip.ParsePrefix(data[0].Subnets[0].Subnet)
		if e != nil || se != nil || !gateway.Is4() || !subnet.Contains(gateway.Next()) {
			err = fmt.Errorf("invalid Podman IPv4 gateway/subnet")
			return
		}
		prepared.Gateway, prepared.IP = gateway.String(), gateway.Next().String()
		network = name + ":ip=" + prepared.IP
		for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			env = append(env, key+"=http://"+prepared.Gateway+":3128")
		}
		env = append(env, "NO_PROXY=", "no_proxy=")
	}
	command := s.Command
	if s.Mode == "session" {
		command = []string{"/cell-init", "idle"}
	}
	args := []string{"create", "--name=cell-" + id, "--label=" + Label + "=" + id,
		"--runtime=" + PodmanRuntime, "--network=" + network,
		"--user=65532:65532", "--workdir=/workspace", "--entrypoint=", "--read-only", "--read-only-tmpfs=false",
		"--cap-drop=ALL", "--security-opt=no-new-privileges", "--systemd=false",
		"--memory=" + strconv.FormatInt(s.MemoryMiB, 10) + "m", "--memory-swap=" + strconv.FormatInt(s.MemoryMiB, 10) + "m",
		"--cpus=" + strconv.FormatFloat(s.CPU, 'f', -1, 64), "--pids-limit=" + strconv.FormatInt(s.PIDs+128, 10),
		fmt.Sprintf("--ulimit=nproc=%d:%d", s.PIDs, s.PIDs), "--ulimit=nofile=1024:1024",
		fmt.Sprintf("--ulimit=fsize=%d:%d", s.WorkspaceMiB<<20, s.WorkspaceMiB<<20),
		fmt.Sprintf("--tmpfs=/workspace:rw,nosuid,nodev,size=%dm,mode=1777", s.WorkspaceMiB),
		"--tmpfs=/tmp:rw,nosuid,nodev,noexec,size=16m,mode=1777",
		"--mount=type=bind,src=" + InitPath + ",dst=/cell-init,ro=true",
		"--log-driver=k8s-file", "--log-opt=max-size=4m", "--restart=no"}
	if network != "none" {
		args = append(args, "--dns=127.0.0.1")
	}
	for _, value := range env {
		args = append(args, "--env="+value)
	}
	args = append(args, "--", images[0].ID)
	args = append(args, command...)
	b, err := podmanOutput(ctx, args...)
	if err == nil {
		prepared.ContainerID = strings.TrimSpace(string(b))
	}
	return
}

func (p *Podman) Start(ctx context.Context, id string) error {
	_, err := podmanOutput(ctx, "start", id)
	return err
}

func (p *Podman) Running(ctx context.Context, id string) (bool, error) {
	b, err := podmanOutput(ctx, "inspect", "--format={{.State.Running}}", id)
	return strings.TrimSpace(string(b)) == "true", err
}

func (p *Podman) Kill(ctx context.Context, id string) error {
	_, err := podmanOutput(ctx, "kill", "--signal=KILL", id)
	return err
}

func (p *Podman) Remove(ctx context.Context, prepared Prepared) error {
	if prepared.ContainerID != "" {
		if _, err := podmanOutput(ctx, "rm", "--force", "--ignore", "--volumes", prepared.ContainerID); err != nil {
			return err
		}
	}
	if prepared.NetworkID != "" {
		if _, err := podmanOutput(ctx, "network", "exists", prepared.NetworkID); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				return nil
			}
			return err
		}
		_, err := podmanOutput(ctx, "network", "rm", prepared.NetworkID)
		return err
	}
	return nil
}

func (p *Podman) Wait(ctx context.Context, id string) (int, error) {
	b, err := podmanOutput(ctx, "wait", id)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

func (p *Podman) Logs(ctx context.Context, id string, out, stderr io.Writer) error {
	cmd := podmanCommand(ctx, "logs", "--follow", id)
	cmd.Stdout, cmd.Stderr = out, stderr
	return cmd.Run()
}

func (p *Podman) Exec(ctx context.Context, id string, command []string, stdin io.Reader, out, stderr io.Writer) (int, error) {
	args := []string{"exec", "--user=65532:65532", "--workdir=/workspace"}
	if stdin != nil {
		args = append(args, "--interactive")
	}
	args = append(args, id)
	args = append(args, command...)
	cmd := podmanCommand(ctx, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, out, stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() != 125 {
		return exit.ExitCode(), nil
	}
	return 0, err
}

func (p *Podman) Reconcile(ctx context.Context) error {
	b, err := podmanOutput(ctx, "ps", "--all", "--quiet", "--no-trunc", "--filter=label="+Label)
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(string(b)) {
		if err = p.Remove(ctx, Prepared{ContainerID: id}); err != nil {
			return err
		}
	}
	b, err = podmanOutput(ctx, "network", "ls", "--format={{.Name}}", "--filter=label="+Label)
	if err != nil {
		return err
	}
	for _, name := range strings.Fields(string(b)) {
		if err = p.Remove(ctx, Prepared{NetworkID: name}); err != nil {
			return err
		}
	}
	return nil
}
