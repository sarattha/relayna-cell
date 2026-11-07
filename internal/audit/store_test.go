package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRestartAndCellIsolation(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Emit("one", "", "runtime", "process.exec", map[string]string{"path": "/bin/sh"}); e != nil {
		t.Fatal(e)
	}
	if e = s.Emit("two", "", "workload", "output", map[string]string{"text": "{\"source\":\"runtime\"}"}); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ev, e := s.Events("one", 0, 10)
	if e != nil || len(ev) != 1 || ev[0].Sequence != 1 {
		t.Fatalf("recovery %v %v", ev, e)
	}
	ev, e = s.Events("", 1, 10)
	if e != nil || len(ev) != 1 || ev[0].Source != "workload" {
		t.Fatalf("envelope/cursor %v %v", ev, e)
	}
	if e = s.Emit("one", "", "control", "cell.deleted", nil); e != nil {
		t.Fatal(e)
	}
	files, e := filepath.Glob(filepath.Join(dir, "logs", "*.ndjson"))
	if e != nil || len(files) != 2 {
		t.Fatalf("segments: %v %v", files, e)
	}
}
func TestExportFailurePreservesDatabaseAndStopsNewEvents(t *testing.T) {
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = os.Remove(s.dir); e != nil {
		t.Fatal(e)
	}
	if e = s.Emit("one", "", "runtime", "process.exec", nil); e == nil {
		t.Fatal("missing export directory accepted")
	}
	if s.Healthy() == nil {
		t.Fatal("audit failure not latched")
	}
	events, e := s.Events("one", 0, 10)
	if e != nil || len(events) != 1 {
		t.Fatalf("committed event lost: %v %v", events, e)
	}
	if e = s.Emit("one", "", "runtime", "process.exec", nil); e == nil {
		t.Fatal("accepted event after audit failure")
	}
}
