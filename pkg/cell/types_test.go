package cell

import "testing"

func TestRestrictedSpec(t *testing.T) {
	base := Spec{Image: "python:3.13-slim"}
	base.Defaults()
	if e := base.Validate(); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*Spec){func(s *Spec) { s.Mode = "agent" }, func(s *Spec) { s.CPU = 100 }, func(s *Spec) { s.MemoryMiB = -1 }, func(s *Spec) { s.WorkspaceMiB = s.MemoryMiB }, func(s *Spec) { s.TimeoutSeconds = -1 }, func(s *Spec) { s.Allow = []Endpoint{{Host: "*.example.com", Port: 443}} }, func(s *Spec) { s.Command = []string{"sh", "-c", "a\x00b"} }} {
		s := base
		mutate(&s)
		if e := s.Validate(); e == nil {
			t.Fatalf("accepted invalid spec: %+v", s)
		}
	}
}
