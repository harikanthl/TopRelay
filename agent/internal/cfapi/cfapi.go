// Package cfapi is a small Cloudflare REST API client with retries. It never logs or returns the token.
package cfapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the Cloudflare API v4 root.
const DefaultBaseURL = "https://api.cloudflare.com/client/v4"

// Cloudflare error codes the deployer handles.
const (
	CodeNoSubdomain    = 10007 // also "script not found" on script routes
	CodeSubdomainTaken = 10031
)

// Client calls the Cloudflare API.
type Client struct {
	BaseURL    string
	Token      string
	HTTP       *http.Client
	MaxRetries int
	// Sleep waits between retries; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}

// New returns a client for token. baseURL may be empty for the default.
func New(token, baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 60 * time.Second}, MaxRetries: 4}
}

// Message is one entry of a Cloudflare error list.
type Message struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// APIError is a failed API call.
type APIError struct {
	Method string
	Path   string
	Status int
	Errors []Message
}

func (e *APIError) Error() string {
	var parts []string
	for _, m := range e.Errors {
		parts = append(parts, fmt.Sprintf("%d %s", m.Code, m.Message))
	}
	detail := strings.Join(parts, "; ")
	if detail == "" {
		detail = http.StatusText(e.Status)
	}
	hint := ""
	switch e.Status {
	case http.StatusUnauthorized:
		hint = " (the Cloudflare token is invalid or expired: run `toprelay login`)"
	case http.StatusForbidden:
		hint = " (the Cloudflare token lacks a permission: it needs Account › Workers Scripts › Edit, Workers KV Storage › Edit, Account Settings › Read; run `toprelay login` with a new token)"
	}
	return fmt.Sprintf("cloudflare %s %s: HTTP %d: %s%s", e.Method, e.Path, e.Status, detail, hint)
}

// HasCode reports whether the error carries a Cloudflare error code.
func (e *APIError) HasCode(code int) bool {
	for _, m := range e.Errors {
		if m.Code == code {
			return true
		}
	}
	return false
}

// IsCode reports whether err is an APIError with code.
func IsCode(err error, code int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.HasCode(code)
}

type envelope struct {
	Success    bool            `json:"success"`
	Errors     []Message       `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

type request struct {
	method  string
	path    string
	query   url.Values
	body    []byte
	ctype   string
	headers map[string]string
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RetryDelay computes the wait before retry attempt n (0-based), honouring Retry-After.
func RetryDelay(n int, retryAfter string, now time.Time) time.Duration {
	if retryAfter != "" {
		if s, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && s >= 0 {
			return min(time.Duration(s)*time.Second, 2*time.Minute)
		}
		if t, err := http.ParseTime(retryAfter); err == nil {
			d := t.Sub(now)
			if d < 0 {
				d = 0
			}
			return min(d, 2*time.Minute)
		}
	}
	base := time.Second << min(n, 5)
	return base/2 + rand.N(base/2+1)
}

// do runs a request with retries on 429 and 5xx; it returns the raw response body and status.
func (c *Client) do(ctx context.Context, r request) (int, http.Header, []byte, error) {
	u := c.BaseURL + r.path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	for attempt := 0; ; attempt++ {
		var body io.Reader
		if r.body != nil {
			body = bytes.NewReader(r.body)
		}
		req, err := http.NewRequestWithContext(ctx, r.method, u, body)
		if err != nil {
			return 0, nil, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("User-Agent", "toprelay-agent")
		if r.ctype != "" {
			req.Header.Set("Content-Type", r.ctype)
		}
		for k, v := range r.headers {
			req.Header.Set(k, v)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil || attempt >= c.MaxRetries {
				return 0, nil, nil, fmt.Errorf("cloudflare %s %s: %w", r.method, r.path, scrubURLError(err))
			}
			if err := c.sleep(ctx, RetryDelay(attempt, "", time.Now())); err != nil {
				return 0, nil, nil, err
			}
			continue
		}
		b, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < c.MaxRetries {
			if err := c.sleep(ctx, RetryDelay(attempt, resp.Header.Get("Retry-After"), time.Now())); err != nil {
				return 0, nil, nil, err
			}
			continue
		}
		if rerr != nil {
			return 0, nil, nil, fmt.Errorf("cloudflare %s %s: read response: %w", r.method, r.path, rerr)
		}
		return resp.StatusCode, resp.Header, b, nil
	}
}

// scrubURLError drops the URL from transport errors; it never holds the token, but keeps messages short.
func scrubURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// call runs a JSON API call and decodes result into out (if non-nil).
func (c *Client) call(ctx context.Context, r request, out any) (*envelope, error) {
	status, _, b, err := c.do(ctx, r)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(b, &env); err != nil {
		if status >= 300 {
			return nil, &APIError{Method: r.method, Path: r.path, Status: status}
		}
		return nil, fmt.Errorf("cloudflare %s %s: unexpected response (HTTP %d)", r.method, r.path, status)
	}
	if status >= 300 || !env.Success {
		return nil, &APIError{Method: r.method, Path: r.path, Status: status, Errors: env.Errors}
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return nil, fmt.Errorf("cloudflare %s %s: decode result: %w", r.method, r.path, err)
		}
	}
	return &env, nil
}

func jsonBody(v any) []byte {
	// Only called with maps of strings and bools, which always marshal.
	b, _ := json.Marshal(v)
	return b
}

// TokenStatus is the result of verifying a token.
type TokenStatus struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	ExpiresOn string `json:"expires_on"`
}

// VerifyToken checks the token with GET /user/tokens/verify.
func (c *Client) VerifyToken(ctx context.Context) (*TokenStatus, error) {
	var ts TokenStatus
	if _, err := c.call(ctx, request{method: "GET", path: "/user/tokens/verify"}, &ts); err != nil {
		return nil, err
	}
	if ts.Status != "active" {
		return &ts, fmt.Errorf("the Cloudflare token is %s, not active: create a new one and run `toprelay login`", ts.Status)
	}
	return &ts, nil
}

// Account is a Cloudflare account.
type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListAccounts returns every account the token can see.
func (c *Client) ListAccounts(ctx context.Context) ([]Account, error) {
	var all []Account
	for page := 1; ; page++ {
		var accts []Account
		env, err := c.call(ctx, request{method: "GET", path: "/accounts", query: url.Values{"per_page": {"50"}, "page": {strconv.Itoa(page)}}}, &accts)
		if err != nil {
			return nil, err
		}
		all = append(all, accts...)
		if env.ResultInfo == nil || page >= env.ResultInfo.TotalPages || len(accts) == 0 {
			return all, nil
		}
	}
}

// GetSubdomain returns the account's workers.dev subdomain. A missing one is an APIError with CodeNoSubdomain.
func (c *Client) GetSubdomain(ctx context.Context, account string) (string, error) {
	var r struct {
		Subdomain string `json:"subdomain"`
	}
	if _, err := c.call(ctx, request{method: "GET", path: "/accounts/" + account + "/workers/subdomain"}, &r); err != nil {
		return "", err
	}
	if r.Subdomain == "" {
		return "", &APIError{Method: "GET", Path: "/accounts/" + account + "/workers/subdomain", Status: 404, Errors: []Message{{Code: CodeNoSubdomain, Message: "no workers.dev subdomain"}}}
	}
	return r.Subdomain, nil
}

// PutSubdomain registers the account's workers.dev subdomain.
func (c *Client) PutSubdomain(ctx context.Context, account, name string) (string, error) {
	var r struct {
		Subdomain string `json:"subdomain"`
	}
	_, err := c.call(ctx, request{method: "PUT", path: "/accounts/" + account + "/workers/subdomain",
		body: jsonBody(map[string]string{"subdomain": name}), ctype: "application/json"}, &r)
	if err != nil {
		return "", err
	}
	if r.Subdomain == "" {
		r.Subdomain = name
	}
	return r.Subdomain, nil
}

// KVNamespace is a Workers KV namespace.
type KVNamespace struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// ListKVNamespaces returns every KV namespace in the account.
func (c *Client) ListKVNamespaces(ctx context.Context, account string) ([]KVNamespace, error) {
	var all []KVNamespace
	for page := 1; ; page++ {
		var ns []KVNamespace
		env, err := c.call(ctx, request{method: "GET", path: "/accounts/" + account + "/storage/kv/namespaces",
			query: url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}}, &ns)
		if err != nil {
			return nil, err
		}
		all = append(all, ns...)
		if env.ResultInfo == nil || page >= env.ResultInfo.TotalPages || len(ns) == 0 {
			return all, nil
		}
	}
}

// CreateKVNamespace creates a namespace and returns it.
func (c *Client) CreateKVNamespace(ctx context.Context, account, title string) (*KVNamespace, error) {
	var ns KVNamespace
	if _, err := c.call(ctx, request{method: "POST", path: "/accounts/" + account + "/storage/kv/namespaces",
		body: jsonBody(map[string]string{"title": title}), ctype: "application/json"}, &ns); err != nil {
		return nil, err
	}
	return &ns, nil
}

// ScriptExists reports whether a Worker script exists (GET …/workers/scripts/{name}).
func (c *Client) ScriptExists(ctx context.Context, account, name string) (bool, error) {
	path := "/accounts/" + account + "/workers/scripts/" + name
	status, _, b, err := c.do(ctx, request{method: "GET", path: path})
	if err != nil {
		return false, err
	}
	switch {
	case status == http.StatusOK:
		return true, nil
	case status == http.StatusNotFound:
		return false, nil
	default:
		var env envelope
		_ = json.Unmarshal(b, &env)
		ae := &APIError{Method: "GET", Path: path, Status: status, Errors: env.Errors}
		if ae.HasCode(CodeNoSubdomain) {
			return false, nil
		}
		return false, ae
	}
}

// ListScripts returns the names of the account's Worker scripts.
func (c *Client) ListScripts(ctx context.Context, account string) ([]string, error) {
	var scripts []struct {
		ID string `json:"id"`
	}
	if _, err := c.call(ctx, request{method: "GET", path: "/accounts/" + account + "/workers/scripts"}, &scripts); err != nil {
		return nil, err
	}
	var names []string
	for _, s := range scripts {
		names = append(names, s.ID)
	}
	return names, nil
}

// Module is one file of a Worker upload.
type Module struct {
	Name        string
	ContentType string
	Content     []byte
}

// PutScript uploads a Worker as multipart: a "metadata" part and one part per module.
func (c *Client) PutScript(ctx context.Context, account, name string, metadata []byte, modules []Module) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="metadata"`)
	h.Set("Content-Type", "application/json")
	pw, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	pw.Write(metadata)
	for _, m := range modules {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, m.Name, m.Name))
		h.Set("Content-Type", m.ContentType)
		pw, err := mw.CreatePart(h)
		if err != nil {
			return err
		}
		pw.Write(m.Content)
	}
	if err := mw.Close(); err != nil {
		return err
	}
	_, err = c.call(ctx, request{method: "PUT", path: "/accounts/" + account + "/workers/scripts/" + name,
		body: buf.Bytes(), ctype: mw.FormDataContentType()}, nil)
	return err
}

// EnableScriptSubdomain turns on the script's workers.dev route.
func (c *Client) EnableScriptSubdomain(ctx context.Context, account, name string) error {
	_, err := c.call(ctx, request{method: "POST", path: "/accounts/" + account + "/workers/scripts/" + name + "/subdomain",
		body: jsonBody(map[string]bool{"enabled": true}), ctype: "application/json",
		headers: map[string]string{"Cloudflare-Workers-Script-Api-Date": "2025-08-01"}}, nil)
	return err
}
