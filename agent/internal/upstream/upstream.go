package upstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/caller"
	"github.com/harikanthl/toprelay/agent/internal/redact"
)

// Credential is the header the agent adds so the app accepts the request: a value from the keyring, or a file
// the app owns, read on every request.
type Credential struct {
	Name  string
	Value string // used when File is ""
	File  string

	last atomic.Pointer[string] // the file value last registered for redaction
}

// redactOnce registers v for redaction unless it was the last value registered here: files are read on every
// request, and the redaction list is global and only grows.
func redactOnce(last *atomic.Pointer[string], v string) {
	if p := last.Load(); p != nil && *p == v {
		return
	}
	redact.Add(v)
	last.Store(&v)
}

// Get returns the header name and its current value.
func (c *Credential) Get() (string, string, error) {
	if c == nil || c.Name == "" {
		return "", "", nil
	}
	if c.File == "" {
		return c.Name, c.Value, nil
	}
	v, err := ReadSecretFile(c.File)
	if err != nil {
		return "", "", err
	}
	redactOnce(&c.last, v)
	return c.Name, v, nil
}

// Options builds an Upstream.
type Options struct {
	Endpoint *Endpoint
	// Pin, if set, is checked before every new connection (macOS).
	Pin *Pin
	// Credential, if set, is added to every request.
	Credential *Credential
	// CallerKeyFile, if set, signs relayed requests with X-TopRelay-Caller.
	CallerKeyFile string
	// Now is the clock for signatures; default time.Now.
	Now func() time.Time
}

// Upstream reaches one local app.
type Upstream struct {
	Endpoint  *Endpoint
	pin       *Pin
	cred      atomic.Pointer[Credential]
	keyFile   atomic.Pointer[string]
	lastKey   atomic.Pointer[string]
	now       func() time.Time
	transport *http.Transport
}

// New builds an Upstream and its transport.
func New(o Options) (*Upstream, error) {
	if o.Endpoint == nil {
		return nil, errors.New("no endpoint")
	}
	if o.Pin != nil {
		if err := o.Pin.Validate(); err != nil {
			return nil, err
		}
		if o.Endpoint.Socket == "" && !o.Endpoint.IsLoopback() {
			return nil, errors.New("a code-signature pin only applies to a local app: use a 127.0.0.1, localhost or unix:// URL")
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	u := &Upstream{Endpoint: o.Endpoint, pin: o.Pin, now: o.Now}
	u.SetCredential(o.Credential)
	u.SetCallerKeyFile(o.CallerKeyFile)
	t := &http.Transport{Proxy: nil, DisableCompression: true, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second,
		ForceAttemptHTTP2: false}
	var d net.Dialer
	switch {
	case o.Endpoint.Socket != "":
		sock, pin := o.Endpoint.Socket, o.Pin
		t.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return dialUnix(ctx, sock, pin) }
	case o.Pin != nil:
		ps, err := o.Endpoint.Port()
		if err != nil {
			return nil, err
		}
		appPort, err := strconv.Atoi(ps)
		if err != nil {
			return nil, err
		}
		pin := o.Pin
		t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			// Who holds the other end of this very connection.
			tc, _ := c.LocalAddr().(*net.TCPAddr)
			if tc == nil {
				c.Close()
				return nil, fmt.Errorf("%w: not a TCP connection", ErrPin)
			}
			pid, err := pin.acceptorPID(ctx, tc.Port, appPort)
			if err == nil {
				err = pin.check(ctx, pid)
			}
			if err != nil {
				c.Close()
				return nil, err
			}
			return c, nil
		}
	default:
		t.DialContext = d.DialContext
	}
	u.transport = t
	return u, nil
}

// Dial opens and closes one connection to the app, with every endpoint check (socket owner, peer user, pin).
func (u *Upstream) Dial(ctx context.Context) error {
	addr := u.Endpoint.URL.Host
	if u.Endpoint.Socket == "" && u.Endpoint.URL.Port() == "" {
		p, _ := u.Endpoint.Port()
		addr = net.JoinHostPort(u.Endpoint.URL.Hostname(), p)
	}
	c, err := u.transport.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

// SetCredential swaps the credential; requests after this use the new one. nil removes it.
func (u *Upstream) SetCredential(c *Credential) {
	if c != nil && c.File == "" && c.Value != "" {
		redact.Add(c.Value)
	}
	u.cred.Store(c)
}

// SetCallerKeyFile swaps the caller key file ("" stops signing).
func (u *Upstream) SetCallerKeyFile(path string) { u.keyFile.Store(&path) }

// Credential returns the current credential (nil if none).
func (u *Upstream) Credential() *Credential { return u.cred.Load() }

// Transport is the RoundTripper that reaches the app (with the endpoint checks); it adds no headers.
func (u *Upstream) Transport() http.RoundTripper { return u.transport }

// CloseIdle drops idle connections (after a config change).
func (u *Upstream) CloseIdle() { u.transport.CloseIdleConnections() }

// ErrCredential wraps a failure to produce the credential or the signature. Remote callers get a generic
// message; the details go to the local log.
var ErrCredential = errors.New("upstream credential")

// PrepareLocal adds the credential to a request the agent makes on its own behalf (tool discovery, doctor, test).
// It removes any X-TopRelay-Caller header, so such requests are never mistaken for relayed ones.
func (u *Upstream) PrepareLocal(r *http.Request) error {
	r.Header.Del(caller.Header)
	name, value, err := u.cred.Load().Get()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCredential, err)
	}
	if name != "" {
		r.Header.Set(name, value)
	}
	return nil
}

// PrepareRelayed readies a request that came from a bot: it drops any X-TopRelay-Caller the bot side sent, adds
// the credential, and signs the request for the app. body is the exact body that will be sent. The client is the
// relay's X-Toprelay-Client header.
func (u *Upstream) PrepareRelayed(r *http.Request, body []byte) error {
	if err := u.PrepareLocal(r); err != nil {
		return err
	}
	path := *u.keyFile.Load()
	if path == "" {
		return nil
	}
	key, err := ReadSecretFile(path)
	if err != nil {
		return fmt.Errorf("%w: caller key: %v", ErrCredential, err)
	}
	if len(key) < caller.MinKeyLen {
		return fmt.Errorf("%w: caller key in %s is shorter than %d characters", ErrCredential, path, caller.MinKeyLen)
	}
	redactOnce(&u.lastKey, key)
	nonce, err := caller.NewNonce()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCredential, err)
	}
	r.Header.Set(caller.Header, caller.Sign([]byte(key), r.Method, r.URL.RequestURI(), r.Header.Get("X-Toprelay-Client"),
		u.now().Unix(), nonce, body))
	return nil
}

// Client is an http.Client for the agent's own requests: endpoint checks plus the credential.
func (u *Upstream) Client() *http.Client {
	return &http.Client{Transport: localTransport{u}}
}

type localTransport struct{ u *Upstream }

func (t localTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if err := t.u.PrepareLocal(r); err != nil {
		if r.Body != nil {
			r.Body.Close()
		}
		return nil, err
	}
	return t.u.transport.RoundTrip(r)
}
