package runtime

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os/exec"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	dnet "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/sarattha/relayna-cell/pkg/cell"
)

const RuntimeName = "runsc-cell"
const Label = "dev.relayna.cell"
const InitPath = "/opt/relayna-cell/bin/cell-init"

type Docker struct{ Client *client.Client }
type Prepared struct{ ContainerID, NetworkID, IP, Gateway string }

func New() (*Docker, error) {
	c, e := client.NewClientWithOpts(client.WithHost("unix:///var/run/docker.sock"), client.WithAPIVersionNegotiation())
	return &Docker{Client: c}, e
}
func (d *Docker) Check(ctx context.Context) error {
	i, e := d.Client.Info(ctx, client.InfoOptions{})
	if e != nil {
		return e
	}
	if i.Info.OSType != "linux" {
		return fmt.Errorf("Linux Docker host is required")
	}
	if _, ok := i.Info.Runtimes[RuntimeName]; !ok {
		return fmt.Errorf("required runtime %s is unavailable", RuntimeName)
	}
	for _, args := range [][]string{{"iptables", "-C", "INPUT", "-i", "rc+", "-p", "tcp", "--dport", "3128", "-j", "ACCEPT"}, {"iptables", "-C", "INPUT", "-i", "rc+", "-j", "DROP"}, {"iptables", "-C", "DOCKER-USER", "-i", "rc+", "-j", "DROP"}, {"ip6tables", "-C", "INPUT", "-i", "rc+", "-j", "DROP"}, {"ip6tables", "-C", "FORWARD", "-i", "rc+", "-j", "DROP"}} {
		if b, e := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput(); e != nil {
			return fmt.Errorf("network guard check: %s: %w", b, e)
		}
	}
	return nil
}
func (d *Docker) Prepare(ctx context.Context, id string, s cell.Spec) (p Prepared, err error) {
	if err = d.Check(ctx); err != nil {
		return
	}
	// Images are pulled by the trusted runtime, before the workload's network exists.
	if _, e := d.Client.ImageInspect(ctx, s.Image); e != nil {
		var pull client.ImagePullResponse
		pull, err = d.Client.ImagePull(ctx, s.Image, client.ImagePullOptions{})
		if err != nil {
			return
		}
		err = pull.Wait(ctx)
		pull.Close()
		if err != nil {
			return
		}
	}
	img, e := d.Client.ImageInspect(ctx, s.Image)
	if e != nil {
		err = e
		return
	}
	// Image-declared volumes would create writable mounts outside the managed workspace.
	if img.Config != nil && len(img.Config.Volumes) > 0 {
		err = fmt.Errorf("image-declared volumes are unsupported")
		return
	}
	mode := container.NetworkMode("none")
	env := []string{"HOME=/workspace", "TMPDIR=/tmp"}
	ncfg := &dnet.NetworkingConfig{}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			d.Remove(cleanup, p)
		}
	}()
	if len(s.Allow) > 0 {
		ipv6 := false
		n, e := d.Client.NetworkCreate(ctx, "cell-"+id, client.NetworkCreateOptions{Driver: "bridge", Internal: true, EnableIPv6: &ipv6, Labels: map[string]string{Label: id}, Options: map[string]string{"com.docker.network.bridge.name": "rc" + id[:10], "com.docker.network.bridge.enable_icc": "false"}})
		if e != nil {
			err = e
			return
		}
		p.NetworkID = n.ID
		ni, e := d.Client.NetworkInspect(ctx, n.ID, client.NetworkInspectOptions{})
		if e != nil {
			err = e
			return
		}
		if len(ni.Network.IPAM.Config) != 1 {
			err = fmt.Errorf("unexpected network IPAM configuration")
			return
		}
		p.Gateway = ni.Network.IPAM.Config[0].Gateway.String()
		ip := net.ParseIP(p.Gateway).To4()
		if ip == nil {
			err = fmt.Errorf("IPv4 gateway required")
			return
		}
		ip[3]++
		p.IP = ip.String()
		mode = container.NetworkMode(n.ID)
		ncfg.EndpointsConfig = map[string]*dnet.EndpointSettings{n.ID: {IPAMConfig: &dnet.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr(p.IP)}}}
		proxy := "http://" + net.JoinHostPort(p.Gateway, "3128")
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			env = append(env, k+"="+proxy)
		}
		env = append(env, "NO_PROXY=", "no_proxy=")
	}
	command := s.Command
	if s.Mode == "session" {
		command = []string{"/cell-init", "idle"}
	}
	pids := s.PIDs
	// Guest tasks and host threads are different resources. Leave bounded room
	// for the Sentry, Go runtime, and systrap helpers while RLIMIT_NPROC enforces
	// the requested workload limit inside gVisor.
	hostPIDs := pids + 128
	config := &container.Config{Image: img.ID, User: "65532:65532", WorkingDir: "/workspace", Entrypoint: command, Cmd: []string{}, Env: env, Labels: map[string]string{Label: id}}
	host := &container.HostConfig{Runtime: RuntimeName, NetworkMode: mode, ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, DNS: []netip.Addr{netip.MustParseAddr("127.0.0.1")}, RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled}, LogConfig: container.LogConfig{Type: "local", Config: map[string]string{"max-size": "4m", "max-file": "2"}}, Tmpfs: map[string]string{"/workspace": fmt.Sprintf("rw,nosuid,nodev,size=%dm,mode=1777", s.WorkspaceMiB), "/tmp": "rw,nosuid,nodev,noexec,size=16m,mode=1777"}, Mounts: []mount.Mount{{Type: mount.TypeBind, Source: InitPath, Target: "/cell-init", ReadOnly: true}}, Resources: container.Resources{Memory: s.MemoryMiB << 20, MemorySwap: s.MemoryMiB << 20, NanoCPUs: int64(s.CPU * 1e9), PidsLimit: &hostPIDs, Ulimits: []*container.Ulimit{{Name: "nproc", Soft: pids, Hard: pids}, {Name: "nofile", Soft: 1024, Hard: 1024}, {Name: "fsize", Soft: s.WorkspaceMiB << 20, Hard: s.WorkspaceMiB << 20}}}}
	r, e := d.Client.ContainerCreate(ctx, client.ContainerCreateOptions{Name: "cell-" + id, Config: config, HostConfig: host, NetworkingConfig: ncfg})
	if e != nil {
		err = e
		return
	}
	p.ContainerID = r.ID
	return
}
func (d *Docker) Start(ctx context.Context, id string) error {
	_, e := d.Client.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return e
}

func (d *Docker) Running(ctx context.Context, id string) (bool, error) {
	r, e := d.Client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if e != nil {
		return false, e
	}
	return r.Container.State != nil && r.Container.State.Running, nil
}
func (d *Docker) Kill(ctx context.Context, id string) error {
	_, e := d.Client.ContainerKill(ctx, id, client.ContainerKillOptions{Signal: "SIGKILL"})
	return e
}
func (d *Docker) Remove(ctx context.Context, p Prepared) error {
	if p.ContainerID != "" {
		if _, e := d.Client.ContainerRemove(ctx, p.ContainerID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); e != nil && !errdefs.IsNotFound(e) {
			return e
		}
	}
	if p.NetworkID != "" {
		_, e := d.Client.NetworkRemove(ctx, p.NetworkID, client.NetworkRemoveOptions{})
		if errdefs.IsNotFound(e) {
			return nil
		}
		return e
	}
	return nil
}
func (d *Docker) Wait(ctx context.Context, id string) (int, error) {
	r := d.Client.ContainerWait(ctx, id, client.ContainerWaitOptions{})
	select {
	case x := <-r.Result:
		if x.Error != nil {
			return 0, fmt.Errorf("%s", x.Error.Message)
		}
		return int(x.StatusCode), nil
	case e := <-r.Error:
		return 0, e
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
func (d *Docker) Logs(ctx context.Context, id string, out, stderr io.Writer) error {
	r, e := d.Client.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
	if e != nil {
		return e
	}
	defer r.Close()
	_, e = stdcopy.StdCopy(out, stderr, r)
	return e
}
func (d *Docker) Exec(ctx context.Context, id string, cmd []string, stdin io.Reader, out, stderr io.Writer) (int, error) {
	e, eerr := d.Client.ExecCreate(ctx, id, client.ExecCreateOptions{User: "65532:65532", WorkingDir: "/workspace", Cmd: cmd, AttachStdin: stdin != nil, AttachStdout: true, AttachStderr: true})
	if eerr != nil {
		return 0, eerr
	}
	a, err := d.Client.ExecAttach(ctx, e.ID, client.ExecAttachOptions{})
	if err != nil {
		return 0, err
	}
	defer a.Close()
	stop := context.AfterFunc(ctx, func() { a.Close() })
	defer stop()
	if stdin != nil {
		go func() { io.Copy(a.Conn, stdin); a.CloseWrite() }()
	}
	if _, err = stdcopy.StdCopy(out, stderr, a.Reader); err != nil {
		return 0, err
	}
	for {
		r, err := d.Client.ExecInspect(ctx, e.ID, client.ExecInspectOptions{})
		if err != nil {
			return 0, err
		}
		if !r.Running {
			return r.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Reconcile closes all owned resources after a server restart. Sessions are
// intentionally not resumed across an interval without an attached observer.
func (d *Docker) Reconcile(ctx context.Context) error {
	r, e := d.Client.ContainerList(ctx, client.ContainerListOptions{All: true})
	if e != nil {
		return e
	}
	for _, c := range r.Items {
		if c.Labels[Label] != "" {
			if _, e = d.Client.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); e != nil {
				return e
			}
		}
	}
	ns, e := d.Client.NetworkList(ctx, client.NetworkListOptions{})
	if e != nil {
		return e
	}
	for _, n := range ns.Items {
		if n.Labels[Label] != "" {
			if _, e = d.Client.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{}); e != nil {
				return e
			}
		}
	}
	return nil
}
