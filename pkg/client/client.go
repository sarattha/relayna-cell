// Package client is the Go client for Relayna Cell's Unix-socket API.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"

	"github.com/sarattha/relayna-cell/pkg/cell"
)

type Client struct{ HTTP *http.Client }

func New(socket string) *Client {
	return &Client{HTTP: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}}
}
func (c *Client) Request(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	r, e := http.NewRequestWithContext(ctx, method, "http://cell"+path, body)
	if e != nil {
		return nil, e
	}
	r.Header.Set("Content-Type", "application/json")
	res, e := c.HTTP.Do(r)
	if e != nil {
		return nil, e
	}
	if res.StatusCode >= 300 {
		defer res.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(res.Body, 8192))
		return nil, fmt.Errorf("API %d: %s", res.StatusCode, b)
	}
	return res, nil
}
func (c *Client) Create(ctx context.Context, s cell.Spec) (cell.Cell, error) {
	var out cell.Cell
	b, e := json.Marshal(s)
	if e != nil {
		return out, e
	}
	r, e := c.Request(ctx, "POST", "/v1alpha1/cells", bytes.NewReader(b))
	if e != nil {
		return out, e
	}
	defer r.Body.Close()
	e = json.NewDecoder(r.Body).Decode(&out)
	return out, e
}
func (c *Client) Exec(ctx context.Context, id string, req cell.ExecRequest, receive func(cell.Frame) error) error {
	b, e := json.Marshal(req)
	if e != nil {
		return e
	}
	r, e := c.Request(ctx, "POST", "/v1alpha1/cells/"+url.PathEscape(id)+"/exec", bytes.NewReader(b))
	if e != nil {
		return e
	}
	defer r.Body.Close()
	d := json.NewDecoder(r.Body)
	terminal := false
	for {
		var f cell.Frame
		e = d.Decode(&f)
		if e == io.EOF {
			if !terminal {
				return fmt.Errorf("execution stream ended without exit status")
			}
			return nil
		}
		if e != nil {
			return e
		}
		if f.Error != "" {
			return fmt.Errorf("%s", f.Error)
		}
		if f.ExitCode != nil {
			terminal = true
		}
		if e = receive(f); e != nil {
			return e
		}
	}
}
func (c *Client) Events(ctx context.Context, id string, after int64) ([]cell.Event, error) {
	r, e := c.Request(ctx, "GET", "/v1alpha1/events?cell_id="+url.QueryEscape(id)+"&after="+strconv.FormatInt(after, 10), nil)
	if e != nil {
		return nil, e
	}
	defer r.Body.Close()
	var out []cell.Event
	e = json.NewDecoder(r.Body).Decode(&out)
	return out, e
}
