package network

import (
	"github.com/sarattha/relayna-cell/pkg/cell"
	"net"
	"testing"
)

func TestExactEndpointBoundary(t *testing.T) {
	allow := []cell.Endpoint{{Host: "llm.example.com", Port: 443}}
	for _, target := range []string{"llm.example.com:443", "LLM.EXAMPLE.COM:443"} {
		if _, e := Match(target, allow); e != nil {
			t.Fatalf("allow %s: %v", target, e)
		}
	}
	for _, target := range []string{"llm.example.com:80", "llm.example.com.evil:443", "llm.example.com.:443", "evil@llm.example.com:443", "llm.example.com", "127.0.0.1:443"} {
		if _, e := Match(target, allow); e == nil {
			t.Fatalf("accepted %s", target)
		}
	}
}
func TestResolvedAddressPolicy(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "::1", "169.254.169.254", "fe80::1", "0.0.0.0", "224.0.0.1"} {
		if IPAllowed(net.ParseIP(s), true) {
			t.Fatalf("accepted reserved address %s", s)
		}
	}
	for _, s := range []string{"10.1.2.3", "192.168.5.2", "fd00::1"} {
		if IPAllowed(net.ParseIP(s), false) {
			t.Fatalf("private address accepted without permission: %s", s)
		}
		if !IPAllowed(net.ParseIP(s), true) {
			t.Fatalf("explicit private target denied: %s", s)
		}
	}
	if !IPAllowed(net.ParseIP("93.184.215.14"), false) {
		t.Fatal("public address denied")
	}
}
