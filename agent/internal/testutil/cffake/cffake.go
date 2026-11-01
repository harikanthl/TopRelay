// Package cffake is an in-memory fake of the Cloudflare API endpoints the agent uses, for tests. It records
// every request so tests can assert on exact multipart parts, metadata and headers.
package cffake

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Upload is one recorded script PUT.
type Upload struct {
	Metadata    map[string]any
	MetadataRaw []byte
	Parts       map[string]Part
	PartOrder   []string
}

// Part is one multipart part.
type Part struct {
	ContentType string
	Filename    string
	Body        []byte
}

// Request is one recorded API call.
type Request struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// Fake is the fake API.
type Fake struct {
	Token       string
	Accounts    []map[string]string
	Subdomain   string
	TakenNames  map[string]bool
	KV          []map[string]string
	ScriptExist bool
	Fail429Once bool

	mu       sync.Mutex
	Requests []Request
	Uploads  []Upload
	Enables  []Request
	Server   *httptest.Server
	t        testing.TB
	seen429  bool
	kvSerial int
}

// New starts a fake with one account and no subdomain, KV namespace or script.
func New(t testing.TB) *Fake {
	f := &Fake{Token: "cf-test-token-0123456789", Accounts: []map[string]string{{"id": "acc1", "name": "Hari's Account"}}, TakenNames: map[string]bool{}, t: t}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

// URL is the API base URL.
func (f *Fake) URL() string { return f.Server.URL + "/client/v4" }

func ok(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result,
		"result_info": map[string]int{"page": 1, "total_pages": 1}})
}

func fail(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": code, "message": msg}}})
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, Request{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		fail(w, 401, 1000, "Invalid API Token")
		return
	}
	if f.Fail429Once && !f.seen429 {
		f.seen429 = true
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(429)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/client/v4")
	switch {
	case r.Method == "GET" && p == "/user/tokens/verify":
		ok(w, map[string]string{"id": "tokid", "status": "active"})
	case r.Method == "GET" && p == "/accounts":
		ok(w, f.Accounts)
	case r.Method == "GET" && strings.HasSuffix(p, "/workers/subdomain"):
		if f.Subdomain == "" {
			fail(w, 404, 10007, "This account does not have a workers.dev subdomain")
			return
		}
		ok(w, map[string]string{"subdomain": f.Subdomain})
	case r.Method == "PUT" && strings.HasSuffix(p, "/workers/subdomain"):
		var in map[string]string
		json.Unmarshal(body, &in)
		if f.TakenNames[in["subdomain"]] {
			fail(w, 409, 10031, "subdomain unavailable")
			return
		}
		f.Subdomain = in["subdomain"]
		ok(w, map[string]string{"subdomain": f.Subdomain})
	case r.Method == "GET" && strings.HasSuffix(p, "/storage/kv/namespaces"):
		ok(w, f.KV)
	case r.Method == "POST" && strings.HasSuffix(p, "/storage/kv/namespaces"):
		var in map[string]string
		json.Unmarshal(body, &in)
		f.kvSerial++
		ns := map[string]string{"id": fmt.Sprintf("kv%d", f.kvSerial), "title": in["title"]}
		f.KV = append(f.KV, ns)
		ok(w, ns)
	case r.Method == "GET" && strings.HasSuffix(p, "/workers/scripts"):
		if f.ScriptExist {
			ok(w, []map[string]string{{"id": "toprelay"}})
		} else {
			ok(w, []any{})
		}
	case r.Method == "GET" && strings.HasSuffix(p, "/workers/scripts/toprelay"):
		if !f.ScriptExist {
			fail(w, 404, 10007, "workers.api.error.script_not_found")
			return
		}
		w.Write([]byte("export class Relay {}"))
	case r.Method == "PUT" && strings.HasSuffix(p, "/workers/scripts/toprelay"):
		up, err := parseUpload(r.Header.Get("Content-Type"), body)
		if err != nil {
			f.t.Errorf("bad multipart upload: %v", err)
			fail(w, 400, 10021, err.Error())
			return
		}
		f.Uploads = append(f.Uploads, up)
		f.ScriptExist = true
		ok(w, map[string]string{"id": "toprelay"})
	case r.Method == "POST" && strings.HasSuffix(p, "/workers/scripts/toprelay/subdomain"):
		f.Enables = append(f.Enables, f.Requests[len(f.Requests)-1])
		ok(w, map[string]bool{"enabled": true})
	default:
		fail(w, 404, 7003, "no route for "+r.Method+" "+p)
	}
}

func parseUpload(ctype string, body []byte) (Upload, error) {
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil || mt != "multipart/form-data" {
		return Upload{}, fmt.Errorf("content type %q", ctype)
	}
	mr := multipart.NewReader(strings.NewReader(string(body)), params["boundary"])
	up := Upload{Parts: map[string]Part{}}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Upload{}, err
		}
		b, _ := io.ReadAll(part)
		name := part.FormName()
		up.Parts[name] = Part{ContentType: part.Header.Get("Content-Type"), Filename: part.FileName(), Body: b}
		up.PartOrder = append(up.PartOrder, name)
	}
	meta, okm := up.Parts["metadata"]
	if !okm {
		return Upload{}, fmt.Errorf("no metadata part")
	}
	up.MetadataRaw = meta.Body
	if err := json.Unmarshal(meta.Body, &up.Metadata); err != nil {
		return Upload{}, fmt.Errorf("metadata JSON: %w", err)
	}
	return up, nil
}

// LastUpload returns the most recent script upload.
func (f *Fake) LastUpload() Upload {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Uploads) == 0 {
		f.t.Fatal("no script upload")
	}
	return f.Uploads[len(f.Uploads)-1]
}

// UploadCount returns the number of script uploads.
func (f *Fake) UploadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Uploads)
}

// Binding returns the named binding from an upload's metadata.
func (u Upload) Binding(name string) map[string]any {
	bs, _ := u.Metadata["bindings"].([]any)
	for _, b := range bs {
		m, _ := b.(map[string]any)
		if m["name"] == name {
			return m
		}
	}
	return nil
}
