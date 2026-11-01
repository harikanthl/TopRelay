// Package keys implements the credentials of protocol.md: the per-service agent key and the macaroon-style
// bearer keys (mint, narrow, parse, verify).
package keys

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	agentPrefix  = "tra1."
	bearerPrefix = "trk1."
	// RootSize is the number of random bytes in AGENT_ROOT and BEARER_ROOT.
	RootSize = 32
)

var b64 = base64.RawURLEncoding

var keyIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// NewRoot returns 32 random bytes encoded as base64url without padding.
func NewRoot() (string, error) {
	b := make([]byte, RootSize)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate root secret: %w", err)
	}
	return b64.EncodeToString(b), nil
}

// DecodeRoot decodes a root secret and checks its length.
func DecodeRoot(root string) ([]byte, error) {
	b, err := b64.DecodeString(root)
	if err != nil {
		return nil, fmt.Errorf("root secret is not base64url: %w", err)
	}
	if len(b) != RootSize {
		return nil, fmt.Errorf("root secret has %d bytes, want %d", len(b), RootSize)
	}
	return b, nil
}

func mac(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// AgentKey computes agent_key(service, gen) from the base64url AGENT_ROOT.
func AgentKey(agentRoot, service string, gen int) (string, error) {
	root, err := DecodeRoot(agentRoot)
	if err != nil {
		return "", err
	}
	return agentPrefix + b64.EncodeToString(mac(root, "toprelay/agent/v1/"+service+"/"+strconv.Itoa(gen))), nil
}

// NewKeyID returns a random 16-character lowercase hex key id.
func NewKeyID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate key id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Bearer is a parsed bearer key.
type Bearer struct {
	ID      string
	Caveats []string
	Sig     []byte
}

type payload struct {
	I string   `json:"i"`
	C []string `json:"c"`
}

// String encodes the key in the compact form minters must produce.
func (b Bearer) String() string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	caveats := b.Caveats
	if caveats == nil {
		caveats = []string{}
	}
	// Encoding a struct of two strings cannot fail.
	_ = enc.Encode(payload{I: b.ID, C: caveats})
	p := bytes.TrimRight(buf.Bytes(), "\n")
	return bearerPrefix + b64.EncodeToString(p) + "." + b64.EncodeToString(b.Sig)
}

// Mint creates a bearer key with id and caveats, signed from the base64url BEARER_ROOT.
func Mint(bearerRoot, id string, caveats []string) (Bearer, error) {
	root, err := DecodeRoot(bearerRoot)
	if err != nil {
		return Bearer{}, err
	}
	if !keyIDRe.MatchString(id) {
		return Bearer{}, fmt.Errorf("key id %q must be 16 lowercase hex characters", id)
	}
	sig := mac(root, "toprelay/bearer/v1/"+id)
	for _, c := range caveats {
		sig = mac(sig, c)
	}
	return Bearer{ID: id, Caveats: slices.Clone(caveats), Sig: sig}, nil
}

// Narrow appends caveats to a key without the root secret.
func Narrow(b Bearer, caveats ...string) Bearer {
	out := Bearer{ID: b.ID, Caveats: slices.Clone(b.Caveats), Sig: slices.Clone(b.Sig)}
	for _, c := range caveats {
		out.Caveats = append(out.Caveats, c)
		out.Sig = mac(out.Sig, c)
	}
	return out
}

// Parse decodes a bearer key string. It checks the structure, not the signature.
func Parse(token string) (Bearer, error) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, bearerPrefix) {
		return Bearer{}, errors.New("not a TopRelay bearer key (it must start with trk1.)")
	}
	parts := strings.Split(strings.TrimPrefix(token, bearerPrefix), ".")
	if len(parts) != 2 {
		return Bearer{}, errors.New("malformed bearer key: want trk1.<payload>.<signature>")
	}
	raw, err := b64.DecodeString(parts[0])
	if err != nil {
		return Bearer{}, fmt.Errorf("malformed bearer key payload: %w", err)
	}
	sig, err := b64.DecodeString(parts[1])
	if err != nil {
		return Bearer{}, fmt.Errorf("malformed bearer key signature: %w", err)
	}
	if len(sig) != sha256.Size {
		return Bearer{}, errors.New("malformed bearer key signature: wrong length")
	}
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return Bearer{}, fmt.Errorf("malformed bearer key payload: %w", err)
	}
	if !keyIDRe.MatchString(p.I) {
		return Bearer{}, errors.New("malformed bearer key: bad key id")
	}
	return Bearer{ID: p.I, Caveats: p.C, Sig: sig}, nil
}

// Request describes what a key is being checked against.
type Request struct {
	Service string
	Gen     int
	Now     time.Time
}

// Verify checks the signature and every caveat, failing closed on unknown caveats. It returns the tool
// restriction (nil when the key doesn't restrict tools).
func Verify(bearerRoot string, b Bearer, req Request) ([]string, error) {
	want, err := Mint(bearerRoot, b.ID, b.Caveats)
	if err != nil {
		return nil, err
	}
	if !hmac.Equal(want.Sig, b.Sig) {
		return nil, errors.New("bad signature")
	}
	return CheckCaveats(b.Caveats, req)
}

// CheckCaveats applies the caveat rules of protocol.md to a request.
func CheckCaveats(caveats []string, req Request) ([]string, error) {
	if len(caveats) < 2 {
		return nil, errors.New("key must start with svc= and g= caveats")
	}
	if caveats[0] != "svc="+req.Service {
		return nil, fmt.Errorf("key is for another service (%s)", caveats[0])
	}
	if caveats[1] != "g="+strconv.Itoa(req.Gen) {
		return nil, fmt.Errorf("key is from another generation (%s); it was rotated", caveats[1])
	}
	var tools []string
	restricted := false
	for _, c := range caveats[2:] {
		k, v, ok := strings.Cut(c, "=")
		if !ok {
			return nil, fmt.Errorf("unknown caveat %q", c)
		}
		switch k {
		case "exp":
			exp, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("bad exp caveat %q", c)
			}
			if req.Now.Unix() >= exp {
				return nil, errors.New("key expired")
			}
		case "tools":
			set := SplitTools(v)
			if !restricted {
				tools, restricted = set, true
			} else {
				tools = intersect(tools, set)
			}
		default:
			return nil, fmt.Errorf("unknown caveat %q", c)
		}
	}
	if restricted && tools == nil {
		tools = []string{}
	}
	return tools, nil
}

// Restrictions summarises the caveats of a key for display.
type Restrictions struct {
	Service string
	Gen     string
	Tools   []string // nil: no restriction
	Expires time.Time
}

// Describe summarises a key's caveats without verifying it.
func Describe(b Bearer) Restrictions {
	var r Restrictions
	restricted := false
	for i, c := range b.Caveats {
		k, v, _ := strings.Cut(c, "=")
		switch {
		case i == 0 && k == "svc":
			r.Service = v
		case i == 1 && k == "g":
			r.Gen = v
		case k == "exp":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				t := time.Unix(n, 0)
				if r.Expires.IsZero() || t.Before(r.Expires) {
					r.Expires = t
				}
			}
		case k == "tools":
			set := SplitTools(v)
			if !restricted {
				r.Tools, restricted = set, true
			} else {
				r.Tools = intersect(r.Tools, set)
			}
		}
	}
	if restricted && r.Tools == nil {
		r.Tools = []string{}
	}
	return r
}

// SplitTools splits a comma-separated tool list, dropping empty entries.
func SplitTools(v string) []string {
	var out []string
	for _, t := range strings.Split(v, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func intersect(a, b []string) []string {
	var out []string
	for _, x := range a {
		if slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

// Caveats builds the caveat list for a new key.
func Caveats(service string, gen int, tools []string, expires time.Time) []string {
	c := []string{"svc=" + service, "g=" + strconv.Itoa(gen)}
	return append(c, ExtraCaveats(tools, expires)...)
}

// ExtraCaveats builds the optional caveats (tools, exp) used when minting or narrowing.
func ExtraCaveats(tools []string, expires time.Time) []string {
	var c []string
	if tools != nil {
		c = append(c, "tools="+strings.Join(tools, ","))
	}
	if !expires.IsZero() {
		c = append(c, "exp="+strconv.FormatInt(expires.Unix(), 10))
	}
	return c
}
