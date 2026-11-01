// Package relayapi calls the relay's admin routes (/<s>/admin/*) and /health with the agent key.
package relayapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to one relay.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a client for the relay base URL.
func New(base string) *Client {
	return &Client{BaseURL: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// Status is the admin status body (protocol.md).
type Status struct {
	Service     string `json:"service"`
	Connected   bool   `json:"connected"`
	ConnectedAt int64  `json:"connectedAt"`
	Agent       string `json:"agent"`
	Day         string `json:"day"`
	Requests    int64  `json:"requests"`
	Rejected    int64  `json:"rejected"`
	BytesIn     int64  `json:"bytesIn"`
	BytesOut    int64  `json:"bytesOut"`
	RevokedKeys int    `json:"revokedKeys"`
}

// Error is a failed admin call.
type Error struct {
	Op     string
	Status int
	Body   string
}

func (e *Error) Error() string {
	hint := ""
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		hint = " (the relay rejected the agent key: the deployed roots or gen differ from this computer's; run `toprelay doctor`)"
	case http.StatusNotFound:
		hint = " (the relay doesn't know this service: redeploy with `toprelay add` or check `toprelay list`)"
	}
	body := strings.TrimSpace(e.Body)
	if len(body) > 200 {
		body = body[:200]
	}
	return fmt.Sprintf("relay %s: HTTP %d %s%s", e.Op, e.Status, body, hint)
}

func (c *Client) do(ctx context.Context, method, path, agentKey string, in, out any) (http.Header, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if agentKey != "" {
		req.Header.Set("Authorization", "Bearer "+agentKey)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("can't reach the relay at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return resp.Header, &Error{Op: method + " " + path, Status: resp.StatusCode, Body: string(b)}
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.Header, fmt.Errorf("relay %s: bad JSON: %w", path, err)
		}
	}
	return resp.Header, nil
}

// Status returns the service's status.
func (c *Client) Status(ctx context.Context, service, agentKey string) (*Status, error) {
	var s Status
	_, err := c.do(ctx, "GET", "/"+service+"/admin/status", agentKey, nil, &s)
	return &s, err
}

// Pair registers a pairing code; it returns the expiry.
func (c *Client) Pair(ctx context.Context, service, agentKey, code string, ttl time.Duration) (time.Time, error) {
	var out struct {
		Expires int64 `json:"expires"`
	}
	_, err := c.do(ctx, "POST", "/"+service+"/admin/pair", agentKey, map[string]any{"code": code, "ttl": int(ttl.Seconds())}, &out)
	return time.Unix(out.Expires, 0), err
}

// RevokeKey adds a bearer key id to the service's revocation list; it returns the list size.
func (c *Client) RevokeKey(ctx context.Context, service, agentKey, id string) (int, error) {
	var out struct {
		Revoked int `json:"revoked"`
	}
	_, err := c.do(ctx, "POST", "/"+service+"/admin/revoke-key", agentKey, map[string]string{"id": id}, &out)
	return out.Revoked, err
}

// RevokeOAuth revokes every OAuth grant for the service.
func (c *Client) RevokeOAuth(ctx context.Context, service, agentKey string) (int, error) {
	var out struct {
		Revoked int `json:"revoked"`
	}
	_, err := c.do(ctx, "POST", "/"+service+"/admin/revoke-oauth", agentKey, struct{}{}, &out)
	return out.Revoked, err
}

// Health calls GET /health and returns the relay's Date header (for clock skew).
func (c *Client) Health(ctx context.Context) (time.Time, error) {
	h, err := c.do(ctx, "GET", "/health", "", nil, nil)
	if err != nil {
		return time.Time{}, err
	}
	d, err := http.ParseTime(h.Get("Date"))
	if err != nil {
		return time.Time{}, nil
	}
	return d, nil
}
