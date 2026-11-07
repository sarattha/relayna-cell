// Package observer receives the pinned gVisor SecCheck v1 protocol.
package observer

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/sarattha/relayna-cell/internal/observer/pb"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type Receiver struct {
	l            *net.UnixListener
	mu           sync.Mutex
	conns        map[*net.UnixConn]bool
	closed       bool
	wg           sync.WaitGroup
	Event        func(containerID, kind string, data any) error
	Fault        func(containerID string, err error)
	Disconnected func(containerID string)
}

func Start(path string, event func(string, string, any) error, fault func(string, error), disconnected func(string)) (*Receiver, error) {
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("observer socket already exists: %s", path)
	}
	l, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		l.Close()
		return nil, err
	}
	r := &Receiver{l: l, conns: map[*net.UnixConn]bool{}, Event: event, Fault: fault, Disconnected: disconnected}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			c, e := l.AcceptUnix()
			if e != nil {
				return
			}
			r.mu.Lock()
			if r.closed || len(r.conns) >= 32 {
				r.mu.Unlock()
				c.Close()
				continue
			}
			r.conns[c] = true
			r.wg.Add(1)
			r.mu.Unlock()
			go r.serve(c)
		}
	}()
	return r, nil
}

func (r *Receiver) serve(c *net.UnixConn) {
	containerID := ""
	defer r.wg.Done()
	defer func() {
		c.Close()
		r.mu.Lock()
		delete(r.conns, c)
		closed := r.closed
		r.mu.Unlock()
		if !closed && containerID != "" && r.Disconnected != nil {
			r.Disconnected(containerID)
		}
	}()
	fail := func(err error) {
		r.mu.Lock()
		closed := r.closed
		r.mu.Unlock()
		if !closed {
			r.Fault(containerID, err)
		}
	}
	buf := make([]byte, 1<<20)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, flags, _, err := c.ReadMsgUnix(buf, nil)
	if err != nil {
		fail(err)
		return
	}
	hs := &pb.Handshake{}
	if flags&unix.MSG_TRUNC != 0 || proto.Unmarshal(buf[:n], hs) != nil || hs.Version != 1 {
		fail(fmt.Errorf("unsupported SecCheck handshake"))
		return
	}
	b, _ := proto.Marshal(&pb.Handshake{Version: 1})
	if _, err = c.Write(b); err != nil {
		fail(err)
		return
	}
	c.SetReadDeadline(time.Time{})
	var dropped uint32
	for {
		n, _, flags, _, err = c.ReadMsgUnix(buf, nil)
		if err != nil {
			if err != io.EOF {
				fail(err)
			}
			return
		}
		if n == 0 {
			return
		}
		if flags&unix.MSG_TRUNC != 0 {
			fail(fmt.Errorf("truncated SecCheck packet"))
			return
		}
		kind, id, data, count, err := Decode(buf[:n])
		if err != nil {
			fail(err)
			return
		}
		if containerID == "" && id != "" {
			containerID = id
		}
		if id != "" && id != containerID {
			fail(fmt.Errorf("container identity changed on SecCheck connection"))
			return
		}
		if count != dropped {
			fail(fmt.Errorf("SecCheck dropped %d events", count-dropped))
			dropped = count
		}
		if kind != "" {
			if err = r.Event(containerID, kind, data); err != nil {
				fail(err)
				return
			}
		}
	}
}

func Decode(packet []byte) (kind, id string, data any, dropped uint32, err error) {
	if len(packet) < 8 {
		return "", "", nil, 0, fmt.Errorf("short SecCheck header")
	}
	h := int(binary.LittleEndian.Uint16(packet))
	typ := pb.MessageType(binary.LittleEndian.Uint16(packet[2:]))
	dropped = binary.LittleEndian.Uint32(packet[4:])
	if h < 8 || h > len(packet) {
		return "", "", nil, dropped, fmt.Errorf("invalid SecCheck header length")
	}
	var msg proto.Message
	switch typ {
	case pb.MessageType_MESSAGE_CONTAINER_START:
		msg = &pb.Start{}
		kind = "container.start"
	case pb.MessageType_MESSAGE_SENTRY_EXEC:
		msg = &pb.ExecveInfo{}
		kind = "process.exec"
	case pb.MessageType_MESSAGE_SENTRY_CLONE:
		msg = &pb.CloneInfo{}
		kind = "process.clone"
	case pb.MessageType_MESSAGE_SENTRY_TASK_EXIT:
		msg = &pb.TaskExit{}
		kind = "process.exit"
	case pb.MessageType_MESSAGE_SYSCALL_EXECVE:
		msg = &pb.Execve{}
		kind = "process.exec_attempt"
	case pb.MessageType_MESSAGE_SYSCALL_CONNECT:
		msg = &pb.Connect{}
		kind = "network.connect_attempt"
	default:
		return "", "", nil, dropped, nil
	}
	if err = proto.Unmarshal(packet[h:], msg); err != nil {
		return
	}
	switch m := msg.(type) {
	case *pb.Start:
		id = m.Id
		m.Env = nil
	case *pb.ExecveInfo:
		id = m.GetContextData().GetContainerId()
		m.Env = nil
	case *pb.CloneInfo:
		id = m.GetContextData().GetContainerId()
	case *pb.TaskExit:
		id = m.GetContextData().GetContainerId()
	case *pb.Execve:
		id = m.GetContextData().GetContainerId()
		m.Envv = nil
	case *pb.Connect:
		id = m.GetContextData().GetContainerId()
	}
	var b []byte
	b, err = (protojson.MarshalOptions{UseProtoNames: true}).Marshal(msg)
	if err != nil {
		return
	}
	if len(b) > 128<<10 {
		err = fmt.Errorf("decoded SecCheck event exceeds 128 KiB")
		return
	}
	err = json.Unmarshal(b, &data)
	return
}

func (r *Receiver) Close() {
	r.mu.Lock()
	r.closed = true
	for c := range r.conns {
		c.Close()
	}
	r.mu.Unlock()
	r.l.Close()
	r.wg.Wait()
}
