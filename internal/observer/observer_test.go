package observer

import (
	"encoding/binary"
	"encoding/json"
	"github.com/sarattha/relayna-cell/internal/observer/pb"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
)

func TestDecodeRejectsMalformedPackets(t *testing.T) {
	for _, p := range [][]byte{nil, {1}, {0, 0, 3, 0, 0, 0, 0, 0}, {9, 0, 3, 0, 0, 0, 0, 0}, {8, 0, 3, 0, 0, 0, 0, 0, 255}} {
		if _, _, _, _, e := Decode(p); e == nil {
			t.Fatalf("accepted %v", p)
		}
	}
}
func TestExecIdentityAndEnvironmentRedaction(t *testing.T) {
	payload, e := proto.Marshal(&pb.ExecveInfo{ContextData: &pb.ContextData{ContainerId: "runtime-id", ThreadGroupId: 42}, BinaryPath: "/bin/sh", Argv: []string{"sh", "-c", "echo test"}, Env: []string{"TOKEN=must-not-log"}})
	if e != nil {
		t.Fatal(e)
	}
	p := make([]byte, 8)
	binary.LittleEndian.PutUint16(p, 8)
	binary.LittleEndian.PutUint16(p[2:], uint16(pb.MessageType_MESSAGE_SENTRY_EXEC))
	binary.LittleEndian.PutUint32(p[4:], 2)
	p = append(p, payload...)
	k, id, d, dropped, e := Decode(p)
	if e != nil || k != "process.exec" || id != "runtime-id" || dropped != 2 {
		t.Fatalf("decode: %s %s %d %v", k, id, dropped, e)
	}
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "must-not-log") {
		t.Fatal("environment leaked")
	}
}
func FuzzDecode(f *testing.F) {
	f.Add([]byte{8, 0, 3, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, p []byte) {
		if len(p) > 1<<20 {
			t.Skip()
		}
		Decode(p)
	})
}
