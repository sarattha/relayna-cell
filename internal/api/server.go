package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/sarattha/relayna-cell/internal/engine"
	"github.com/sarattha/relayna-cell/pkg/cell"
)

func Handler(m *engine.Manager) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if e := m.Store.Healthy(); e != nil {
			failure(w, e, 503)
			return
		}
		write(w, map[string]string{"status": "ok", "api_version": cell.APIVersion})
	})
	mux.HandleFunc("POST /v1alpha1/cells", func(w http.ResponseWriter, r *http.Request) {
		var s cell.Spec
		if e := decode(w, r, &s); e != nil {
			failure(w, e, 400)
			return
		}
		c, e := m.Create(r.Context(), s)
		if e != nil {
			failure(w, e, 422)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		write(w, c)
	})
	mux.HandleFunc("GET /v1alpha1/cells", func(w http.ResponseWriter, r *http.Request) {
		cs, e := m.List()
		if e != nil {
			failure(w, e, 500)
			return
		}
		write(w, cs)
	})
	mux.HandleFunc("GET /v1alpha1/cells/{id}", func(w http.ResponseWriter, r *http.Request) {
		c, e := m.Get(r.PathValue("id"))
		if e != nil {
			failure(w, e, 404)
			return
		}
		write(w, c)
	})
	mux.HandleFunc("DELETE /v1alpha1/cells/{id}", func(w http.ResponseWriter, r *http.Request) {
		if e := m.Delete(r.PathValue("id")); e != nil {
			failure(w, e, 409)
			return
		}
		write(w, map[string]string{"status": "deleted"})
	})
	mux.HandleFunc("GET /v1alpha1/events", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		events, e := m.Store.Events(r.URL.Query().Get("cell_id"), after, 1000)
		if e != nil {
			failure(w, e, 500)
			return
		}
		write(w, events)
	})
	mux.HandleFunc("POST /v1alpha1/cells/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var req cell.ExecRequest
		if e := decode(w, r, &req); e != nil {
			failure(w, e, 400)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		send := func(f cell.Frame) error {
			if e := enc.Encode(f); e != nil {
				return e
			}
			return http.NewResponseController(w).Flush()
		}
		if e := m.Exec(r.Context(), r.PathValue("id"), req, send, true); e != nil {
			_ = send(cell.Frame{Stream: "error", Error: e.Error()})
		}
	})
	mux.HandleFunc("PUT /v1alpha1/cells/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		if !filepath.IsLocal(p) {
			failure(w, fmt.Errorf("path must be relative to workspace"), 400)
			return
		}
		b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if e != nil {
			failure(w, e, 400)
			return
		}
		req := cell.ExecRequest{Command: []string{"/cell-init", "write", p}, Stdin: b}
		var stderr bytes.Buffer
		e = m.Exec(r.Context(), r.PathValue("id"), req, func(f cell.Frame) error {
			if f.Stream == "stderr" {
				stderr.Write(f.Data)
			}
			if f.ExitCode != nil && *f.ExitCode != 0 {
				return fmt.Errorf("file write: %s", stderr.String())
			}
			return nil
		}, false)
		if e != nil {
			failure(w, e, 422)
			return
		}
		write(w, map[string]int{"bytes": len(b)})
	})
	mux.HandleFunc("GET /v1alpha1/cells/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		if !filepath.IsLocal(p) {
			failure(w, fmt.Errorf("path must be relative to workspace"), 400)
			return
		}
		var out, stderr bytes.Buffer
		e := m.Exec(r.Context(), r.PathValue("id"), cell.ExecRequest{Command: []string{"/cell-init", "read", p}}, func(f cell.Frame) error {
			if f.Stream == "stdout" {
				out.Write(f.Data)
			}
			if f.Stream == "stderr" {
				stderr.Write(f.Data)
			}
			if f.ExitCode != nil && *f.ExitCode != 0 {
				return fmt.Errorf("file read: %s", stderr.String())
			}
			return nil
		}, false)
		if e != nil {
			failure(w, e, 422)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(out.Bytes())
	})
	return mux
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 6<<20))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}
func write(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
func failure(w http.ResponseWriter, e error, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": e.Error()})
}
