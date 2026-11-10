// Package network enforces exact HTTP/CONNECT destinations outside the sandbox.
// Host firewall rules make this proxy the only permitted non-loopback route.
package network

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sarattha/relayna-cell/pkg/cell"
)

type Grant struct {
	CellID  string
	Allow   []cell.Endpoint
	Context context.Context
}
type Proxy struct {
	mu     sync.RWMutex
	grants map[string]Grant
	slots  chan struct{}
	Log    func(string, string, any) error
}

func New(log func(string, string, any) error) *Proxy {
	return &Proxy{grants: map[string]Grant{}, slots: make(chan struct{}, 64), Log: log}
}
func (p *Proxy) Register(ip string, g Grant) { p.mu.Lock(); defer p.mu.Unlock(); p.grants[ip] = g }
func (p *Proxy) Revoke(cellID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for ip, g := range p.grants {
		if g.CellID == cellID {
			delete(p.grants, ip)
		}
	}
}

func Match(authority string, allow []cell.Endpoint) (cell.Endpoint, error) {
	h, ps, err := net.SplitHostPort(authority)
	if err != nil {
		return cell.Endpoint{}, fmt.Errorf("explicit host:port required")
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		return cell.Endpoint{}, err
	}
	h = strings.ToLower(h)
	for _, e := range allow {
		if h == e.Host && port == e.Port {
			return e, nil
		}
	}
	return cell.Endpoint{}, fmt.Errorf("destination is not allowed")
}
func IPAllowed(ip net.IP, private bool) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	return private || !ip.IsPrivate()
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		http.Error(w, "proxy concurrency limit reached", 429)
		return
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		http.Error(w, "invalid peer", 403)
		return
	}
	p.mu.RLock()
	g, ok := p.grants[ip]
	p.mu.RUnlock()
	if !ok {
		http.Error(w, "unknown Cell", 403)
		return
	}
	ctx, cancel := context.WithTimeout(g.Context, 5*time.Minute)
	defer cancel()
	stop := context.AfterFunc(r.Context(), cancel)
	defer stop()
	authority := r.Host
	if r.Method != http.MethodConnect {
		if r.URL.Scheme != "http" || r.URL.User != nil || r.URL.Host != r.Host {
			http.Error(w, "only absolute HTTP URLs and CONNECT are supported", 400)
			return
		}
		if r.URL.Port() == "" {
			authority = net.JoinHostPort(r.URL.Hostname(), "80")
		}
	}
	e, err := Match(authority, g.Allow)
	if err != nil {
		p.Log(g.CellID, "network.deny", map[string]any{"destination": authority, "reason": err.Error()})
		http.Error(w, "destination denied", 403)
		return
	}
	// Resolve once, validate the result, and dial that IP: no DNS rebinding gap.
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", e.Host)
	if err != nil {
		p.Log(g.CellID, "network.error", map[string]any{"destination": authority, "reason": "DNS lookup failed"})
		http.Error(w, "DNS lookup failed", 502)
		return
	}
	var target net.IP
	for _, candidate := range ips {
		if !IPAllowed(candidate, e.AllowPrivate) {
			p.Log(g.CellID, "network.deny", map[string]any{"destination": authority, "reason": "restricted address"})
			http.Error(w, "restricted address", 403)
			return
		}
		if candidate.To4() != nil {
			target = candidate
		}
	}
	if target == nil && len(ips) > 0 {
		target = ips[0]
	}
	if target == nil {
		http.Error(w, "no address", 502)
		return
	}
	if err = p.Log(g.CellID, "network.allow", map[string]any{"destination": authority, "ip": target.String(), "method": r.Method}); err != nil {
		http.Error(w, "audit unavailable", 503)
		return
	}
	addr := net.JoinHostPort(target.String(), strconv.Itoa(e.Port))
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", addr)
	}
	if r.Method == http.MethodConnect {
		up, err := dial(ctx, "tcp", addr)
		if err != nil {
			http.Error(w, "upstream unavailable", 502)
			return
		}
		defer up.Close()
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking unavailable", 500)
			return
		}
		down, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer down.Close()
		end := context.AfterFunc(ctx, func() { down.Close(); up.Close() })
		defer end()
		if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if err = rw.Flush(); err != nil {
			return
		}
		done := make(chan struct{})
		go func() { io.Copy(up, rw); up.Close(); close(done) }()
		io.Copy(down, up)
		down.Close()
		<-done
		return
	}
	tr := &http.Transport{DialContext: dial, DisableKeepAlives: true, ResponseHeaderTimeout: 30 * time.Second, MaxResponseHeaderBytes: 64 << 10}
	defer tr.CloseIdleConnections()
	req := r.Clone(ctx)
	req.RequestURI = ""
	stripHop(req.Header)
	res, err := tr.RoundTrip(req)
	if err != nil {
		http.Error(w, "upstream unavailable", 502)
		return
	}
	defer res.Body.Close()
	stripHop(res.Header)
	for k, vs := range res.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(res.StatusCode)
	io.Copy(w, res.Body)
}
func stripHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, k := range strings.Split(v, ",") {
			h.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range []string{"Connection", "Proxy-Connection", "Proxy-Authorization", "Proxy-Authenticate", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
}
