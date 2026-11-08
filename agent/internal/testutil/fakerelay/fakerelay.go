// Package fakerelay is an in-process stand-in for the relay Worker + Durable Object, speaking the protocol.md
// frames. Tests use it to drive the tunnel, the handlers and the CLI end to end.
package fakerelay

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/harikanthl/toprelay/agent/internal/keys"
)

// Service is a service the fake relay knows.
type Service struct {
	Name string
	Kind string
	Gen  int
}

// Hello is a received hello frame.
type Hello struct {
	Agent string    `json:"agent"`
	Kind  string    `json:"kind"`
	Tools *[]string `json:"tools"`
}

// Event is something the relay received for a request: the head, a chunk, or the end.
type Event struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Stream  bool              `json:"stream"`
	Data    string            `json:"data"`
	Error   string            `json:"error"`
	At      time.Time         `json:"-"`
}

// Relay is the fake.
type Relay struct {
	AgentRoot  string
	BearerRoot string
	Server     *httptest.Server
	// NoPong stops answering pings (to test the pong timeout).
	NoPong atomic.Bool
	// NoFiles makes the relay act like one deployed before file links (no "files" in welcome).
	NoFiles atomic.Bool

	mu        sync.Mutex
	services  map[string]Service
	sockets   map[string]*socket
	hellos    map[string][]Hello
	connects  map[string]int
	pairs     map[string][]string
	revoked   map[string][]string
	oauthRevs map[string]int
	counter   atomic.Int64
	usedFiles map[string]bool
	t         testing.TB
}

type socket struct {
	ws      *websocket.Conn
	ctx     context.Context
	mu      sync.Mutex
	pending map[string]chan Event
	closed  chan struct{}
}

// New starts a fake relay with fresh roots.
func New(t testing.TB, services ...Service) *Relay {
	ar, _ := keys.NewRoot()
	br, _ := keys.NewRoot()
	r := &Relay{AgentRoot: ar, BearerRoot: br, services: map[string]Service{}, sockets: map[string]*socket{},
		hellos: map[string][]Hello{}, connects: map[string]int{}, pairs: map[string][]string{},
		revoked: map[string][]string{}, oauthRevs: map[string]int{}, t: t}
	for _, s := range services {
		r.services[s.Name] = s
	}
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.Server.Close)
	return r
}

// URL is the relay's base URL (http://…).
func (r *Relay) URL() string { return r.Server.URL }

// SetService adds or replaces a service.
func (r *Relay) SetService(s Service) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.services[s.Name] = s
}

func (r *Relay) agentOK(req *http.Request, svc Service) bool {
	want, err := keys.AgentKey(r.AgentRoot, svc.Name, svc.Gen)
	if err != nil {
		return false
	}
	got := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (r *Relay) serve(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/health" {
		io.WriteString(w, "ok")
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/"), "/", 2)
	r.mu.Lock()
	svc, ok := r.services[parts[0]]
	r.mu.Unlock()
	if !ok {
		http.NotFound(w, req)
		return
	}
	rest := ""
	if len(parts) == 2 {
		rest = parts[1]
	}
	switch {
	case rest == "connect":
		if !r.agentOK(req, svc) {
			http.Error(w, "bad agent key", http.StatusUnauthorized)
			return
		}
		r.accept(w, req, svc)
	case strings.HasPrefix(rest, "admin/"):
		if !r.agentOK(req, svc) {
			http.Error(w, "bad agent key", http.StatusUnauthorized)
			return
		}
		r.admin(w, req, svc, strings.TrimPrefix(rest, "admin/"))
	case svc.Kind == "mcp" && strings.HasPrefix(rest, "files/"):
		r.file(w, req, svc, strings.TrimPrefix(rest, "files/"))
	default:
		r.forward(w, req, svc, "/"+rest)
	}
}

// file is the signed, single-use download route, checked as the Worker does.
func (r *Relay) file(w http.ResponseWriter, req *http.Request, svc Service, id string) {
	exp, err := strconv.ParseInt(req.URL.Query().Get("e"), 10, 64)
	want, serr := keys.FileSig(r.BearerRoot, svc.Name, svc.Gen, id, exp)
	if err != nil || serr != nil || !keys.FileIDRE.MatchString(id) || req.URL.Query().Get("s") != want {
		http.Error(w, "bad link", http.StatusForbidden)
		return
	}
	if exp <= time.Now().Unix() {
		http.Error(w, "expired", http.StatusGone)
		return
	}
	r.mu.Lock()
	if r.usedFiles == nil {
		r.usedFiles = map[string]bool{}
	}
	used := r.usedFiles[id]
	r.usedFiles[id] = true
	r.mu.Unlock()
	if used {
		http.Error(w, "already used", http.StatusGone)
		return
	}
	x, err := r.Send(svc.Name, Request{Method: "GET", Path: "/files/" + id, Internal: "file",
		Headers: map[string]string{"x-forwarded-proto": "https", "x-forwarded-host": req.Host, "x-toprelay-client": "file"}})
	if err != nil {
		http.Error(w, "offline", http.StatusServiceUnavailable)
		return
	}
	resp, err := x.Collect(30 * time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	}
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Disposition", "attachment")
	if cd := resp.Headers["content-disposition"]; strings.HasPrefix(cd, "attachment") {
		w.Header().Set("Content-Disposition", cd)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(resp.Status)
	w.Write(resp.Body)
}

func (r *Relay) accept(w http.ResponseWriter, req *http.Request, svc Service) {
	ws, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(64 << 20)
	ctx := req.Context()
	_, msg, err := ws.Read(ctx)
	if err != nil {
		return
	}
	var h struct {
		Type string `json:"type"`
		Hello
	}
	if json.Unmarshal(msg, &h) != nil || h.Type != "hello" {
		ws.Close(4400, "hello must be first")
		return
	}
	s := &socket{ws: ws, ctx: ctx, pending: map[string]chan Event{}, closed: make(chan struct{})}
	r.mu.Lock()
	if old := r.sockets[svc.Name]; old != nil {
		old.ws.Close(4000, "replaced")
	}
	r.sockets[svc.Name] = s
	r.hellos[svc.Name] = append(r.hellos[svc.Name], h.Hello)
	r.connects[svc.Name]++
	r.mu.Unlock()
	caps := `["files"]`
	if r.NoFiles.Load() {
		caps = `[]`
	}
	ws.Write(ctx, websocket.MessageText, []byte(`{"v":1,"type":"welcome","caps":`+caps+`,"maxBody":20971520}`))
	defer func() {
		close(s.closed)
		r.mu.Lock()
		if r.sockets[svc.Name] == s {
			delete(r.sockets, svc.Name)
		}
		r.mu.Unlock()
		s.mu.Lock()
		for id, ch := range s.pending {
			close(ch)
			delete(s.pending, id)
		}
		s.mu.Unlock()
	}()
	for {
		_, msg, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if string(msg) == "ping" {
			if !r.NoPong.Load() {
				ws.Write(ctx, websocket.MessageText, []byte("pong"))
			}
			continue
		}
		var e Event
		if json.Unmarshal(msg, &e) != nil {
			continue
		}
		e.At = time.Now()
		if e.Type == "hello" {
			var hh Hello
			json.Unmarshal(msg, &hh)
			r.mu.Lock()
			r.hellos[svc.Name] = append(r.hellos[svc.Name], hh)
			r.mu.Unlock()
			continue
		}
		s.mu.Lock()
		ch := s.pending[e.ID]
		s.mu.Unlock()
		if ch != nil {
			ch <- e
			if e.Type == "end" || (e.Type == "response" && !e.Stream) {
				s.mu.Lock()
				delete(s.pending, e.ID)
				s.mu.Unlock()
			}
		}
	}
}

// Connected reports whether the service has a socket.
func (r *Relay) Connected(service string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sockets[service] != nil
}

// WaitConnected waits until the service has a socket and has connected at least n times.
func (r *Relay) WaitConnected(t testing.TB, service string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		ok := r.sockets[service] != nil && r.connects[service] >= n
		r.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("service %s never connected %d times", service, n)
}

// Hellos returns the hello frames received for a service.
func (r *Relay) Hellos(service string) []Hello {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.hellos[service])
}

// Connects returns how many times the service connected.
func (r *Relay) Connects(service string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.connects[service]
}

// Drop closes the service's socket abruptly (a dropped network).
func (r *Relay) Drop(service string) {
	r.mu.Lock()
	s := r.sockets[service]
	r.mu.Unlock()
	if s != nil {
		s.ws.CloseNow()
	}
}

// Request is a request to send to the agent.
type Request struct {
	Method  string
	Path    string
	Query   string
	Headers map[string]string
	Body    []byte
	Tools   *[]string
	// Internal is the frame's "internal" field ("file" for a download).
	Internal string
}

// Exchange is an in-flight request: read Events until the channel closes.
type Exchange struct {
	ID     string
	Events chan Event
	sock   *socket
}

// Send sends a request frame to the service's agent.
func (r *Relay) Send(service string, rq Request) (*Exchange, error) {
	r.mu.Lock()
	s := r.sockets[service]
	r.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("%s is offline", service)
	}
	id := fmt.Sprintf("fake-%d", r.counter.Add(1))
	ch := make(chan Event, 1024)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	if rq.Method == "" {
		rq.Method = "POST"
	}
	if rq.Headers == nil {
		rq.Headers = map[string]string{}
	}
	f := map[string]any{"v": 1, "type": "request", "id": id, "method": rq.Method, "path": rq.Path, "query": rq.Query,
		"headers": rq.Headers, "body": base64.StdEncoding.EncodeToString(rq.Body)}
	if rq.Tools != nil {
		f["tools"] = *rq.Tools
	}
	if rq.Internal != "" {
		f["internal"] = rq.Internal
	}
	b, _ := json.Marshal(f)
	if err := s.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		return nil, err
	}
	return &Exchange{ID: id, Events: ch, sock: s}, nil
}

// Cancel sends a cancel frame for the exchange.
func (x *Exchange) Cancel() error {
	x.sock.mu.Lock()
	delete(x.sock.pending, x.ID)
	x.sock.mu.Unlock()
	return x.sock.ws.Write(context.Background(), websocket.MessageText, []byte(`{"v":1,"type":"cancel","id":"`+x.ID+`"}`))
}

// Response is a collected response.
type Response struct {
	Status  int
	Headers map[string]string
	Body    []byte
	Stream  bool
	Chunks  []Event
	EndErr  string
	Dropped bool // the socket closed before the response finished
}

// Collect waits for a whole response or a stream's end.
func (x *Exchange) Collect(timeout time.Duration) (*Response, error) {
	resp := &Response{}
	deadline := time.After(timeout)
	for {
		select {
		case e, ok := <-x.Events:
			if !ok {
				resp.Dropped = true
				return resp, nil
			}
			switch e.Type {
			case "response":
				resp.Status, resp.Headers, resp.Stream = e.Status, e.Headers, e.Stream
				if !e.Stream {
					resp.Body, _ = base64.StdEncoding.DecodeString(e.Body)
					return resp, nil
				}
			case "chunk":
				d, _ := base64.StdEncoding.DecodeString(e.Data)
				resp.Body = append(resp.Body, d...)
				resp.Chunks = append(resp.Chunks, e)
			case "end":
				resp.EndErr = e.Error
				return resp, nil
			}
		case <-deadline:
			return resp, fmt.Errorf("timeout waiting for response %s", x.ID)
		}
	}
}

// Do sends a request and collects the response.
func (r *Relay) Do(service string, rq Request) (*Response, error) {
	x, err := r.Send(service, rq)
	if err != nil {
		return nil, err
	}
	return x.Collect(10 * time.Second)
}

var forwardHeaders = []string{"accept", "content-type", "user-agent", "mcp-protocol-version", "mcp-session-id",
	"mcp-method", "mcp-name", "last-event-id"}

// forward is the public side: bearer key check, then a request frame, streamed back over HTTP.
func (r *Relay) forward(w http.ResponseWriter, req *http.Request, svc Service, path string) {
	if svc.Kind == "mcp" && path != "/mcp" && !strings.HasPrefix(path, "/mcp/") {
		http.NotFound(w, req)
		return
	}
	tok := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	b, err := keys.Parse(tok)
	var tools []string
	if err == nil {
		tools, err = keys.Verify(r.BearerRoot, b, keys.Request{Service: svc.Name, Gen: svc.Gen, Now: time.Now()})
	}
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+r.URL()+`/.well-known/oauth-protected-resource/`+svc.Name+`"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(req.Body)
	hdr := map[string]string{"x-forwarded-proto": "https", "x-forwarded-host": req.Host,
		"cf-connecting-ip": "203.0.113.7", "x-forwarded-for": "203.0.113.7", "x-toprelay-client": "key:" + b.ID}
	for _, h := range forwardHeaders {
		if v := req.Header.Get(h); v != "" {
			hdr[h] = v
		}
	}
	rq := Request{Method: req.Method, Path: path, Query: req.URL.RawQuery, Headers: hdr, Body: body}
	if tools != nil {
		rq.Tools = &tools
	}
	x, err := r.Send(svc.Name, rq)
	if err != nil {
		http.Error(w, svc.Name+" is offline", http.StatusServiceUnavailable)
		return
	}
	go func() {
		<-req.Context().Done()
		select {
		case <-x.sock.closed:
		default:
			x.Cancel()
		}
	}()
	fl, _ := w.(http.Flusher)
	for {
		select {
		case e, ok := <-x.Events:
			if !ok {
				return
			}
			switch e.Type {
			case "response":
				for k, v := range e.Headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(e.Status)
				if !e.Stream {
					d, _ := base64.StdEncoding.DecodeString(e.Body)
					w.Write(d)
					return
				}
				if fl != nil {
					fl.Flush()
				}
			case "chunk":
				d, _ := base64.StdEncoding.DecodeString(e.Data)
				w.Write(d)
				if fl != nil {
					fl.Flush()
				}
			case "end":
				return
			}
		case <-req.Context().Done():
			return
		}
	}
}

func (r *Relay) admin(w http.ResponseWriter, req *http.Request, svc Service, op string) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case op == "status" && req.Method == "GET":
		r.mu.Lock()
		connected := r.sockets[svc.Name] != nil
		r.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"service": svc.Name, "connected": connected, "connectedAt": 1793500000,
			"agent": "toprelay/0.1.0", "day": time.Now().UTC().Format("2006-01-02"), "requests": 123, "rejected": 4,
			"bytesIn": 5678, "bytesOut": 91011, "revokedKeys": len(r.revoked[svc.Name])})
	case op == "pair" && req.Method == "POST":
		var in struct {
			Code string `json:"code"`
			TTL  int    `json:"ttl"`
		}
		json.NewDecoder(req.Body).Decode(&in)
		if len(in.Code) < 8 || in.TTL < 60 || in.TTL > 3600 {
			http.Error(w, `{"error":"bad pair request"}`, 400)
			return
		}
		r.mu.Lock()
		r.pairs[svc.Name] = append(r.pairs[svc.Name], in.Code)
		r.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]int64{"expires": time.Now().Unix() + int64(in.TTL)})
	case op == "revoke-key" && req.Method == "POST":
		var in struct {
			ID string `json:"id"`
		}
		json.NewDecoder(req.Body).Decode(&in)
		r.mu.Lock()
		r.revoked[svc.Name] = append(r.revoked[svc.Name], in.ID)
		n := len(r.revoked[svc.Name])
		r.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]int{"revoked": n})
	case op == "revoke-oauth" && req.Method == "POST":
		r.mu.Lock()
		r.oauthRevs[svc.Name]++
		r.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]int{"revoked": 2})
	default:
		http.NotFound(w, req)
	}
}

// Pairs returns the pairing codes registered for a service.
func (r *Relay) Pairs(service string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.pairs[service])
}

// Revoked returns the revoked key ids for a service.
func (r *Relay) Revoked(service string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.revoked[service])
}

// OAuthRevocations returns how many times OAuth grants were revoked for a service.
func (r *Relay) OAuthRevocations(service string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.oauthRevs[service]
}
