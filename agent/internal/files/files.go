// Package files lets a local app hand a file to a bot (docs/protocol.md "Files"). The app returns an MCP
// resource_link whose uri is
//
//	toprelay-file:///<relative path>   a file it wrote into the service's outbox directory, or
//	toprelay-file://<offer id>         a file it registered with the agent (control socket POST /v1/files)
//
// and the agent replaces the uri with a signed, single-use https URL on the relay that expires within minutes.
// When the bot fetches it, the relay asks the agent, which streams the file.
package files

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/keys"
)

// Limits.
const (
	MaxFileSize  = 256 << 20
	MaxOffers    = 1024
	MaxDownloads = 4096
	// DefaultLinkTTL is how long a download link works.
	DefaultLinkTTL = 10 * time.Minute
	// DefaultOfferTTL is how long an offered file can still be linked.
	DefaultOfferTTL = time.Hour
)

// Entry is a file that may be downloaded: its path and the identity it had when it was offered.
type Entry struct {
	Service     string
	Path        string
	Name        string
	ContentType string
	Size        int64
	dev, ino    uint64
	mtime       time.Time
}

type offer struct {
	e       *Entry
	expires time.Time
}

type download struct {
	e       *Entry
	client  string
	expires time.Time
}

// Registry holds offered files and outstanding download links, in memory: links die with the agent.
type Registry struct {
	mu        sync.Mutex
	offers    map[string]offer
	downloads map[string]download
	Now       func() time.Time
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{offers: map[string]offer{}, downloads: map[string]download{}, Now: time.Now}
}

// snapshot checks a file and records its identity.
func snapshot(service, p, name, contentType string) (*Entry, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if st.Size() > MaxFileSize {
		return nil, fmt.Errorf("larger than %d MiB", MaxFileSize>>20)
	}
	dev, ino, uid, ok := identity(st)
	if !ok {
		return nil, errors.New("can't read the file's identity on this system")
	}
	if uid != os.Getuid() {
		return nil, errors.New("not owned by you")
	}
	if name == "" {
		name = filepath.Base(p)
	}
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(name))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	}
	return &Entry{Service: service, Path: p, Name: name, ContentType: contentType, Size: st.Size(),
		dev: dev, ino: ino, mtime: st.ModTime()}, nil
}

func (r *Registry) sweepLocked(now time.Time) {
	for k, o := range r.offers {
		if !now.Before(o.expires) {
			delete(r.offers, k)
		}
	}
	for k, d := range r.downloads {
		if !now.Before(d.expires) {
			delete(r.downloads, k)
		}
	}
}

// validMeta checks an offered name and content type: they end up in response headers and in the tool result.
func validMeta(name, contentType string) error {
	if len(name) > 255 || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '/' || r == '\\' }) {
		return errors.New("the name must be at most 255 characters, without control characters or slashes")
	}
	if contentType != "" {
		if len(contentType) > 255 || strings.ContainsFunc(contentType, func(r rune) bool { return r < 0x20 || r >= 0x7f }) {
			return errors.New("the content type has characters that aren't allowed")
		}
		if _, _, err := mime.ParseMediaType(contentType); err != nil {
			return fmt.Errorf("bad content type %q", contentType)
		}
	}
	return nil
}

// Offer registers an absolute path for a service and returns the id for its toprelay-file://<id> uri. The file's
// identity is taken again when a link is made, so the app may rewrite it until then.
func (r *Registry) Offer(service, p, name, contentType string, ttl time.Duration) (string, time.Time, error) {
	if !filepath.IsAbs(p) {
		return "", time.Time{}, errors.New("the path must be absolute")
	}
	if err := validMeta(name, contentType); err != nil {
		return "", time.Time{}, err
	}
	e, err := snapshot(service, p, name, contentType)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%s: %w", p, err)
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		ttl = DefaultOfferTTL
	}
	id, err := keys.NewFileID()
	if err != nil {
		return "", time.Time{}, err
	}
	now := r.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(now)
	if len(r.offers) >= MaxOffers {
		return "", time.Time{}, fmt.Errorf("too many offered files (%d); let some expire", MaxOffers)
	}
	exp := now.Add(ttl)
	r.offers[id] = offer{e: e, expires: exp}
	return id, exp, nil
}

// Resolve finds the file a toprelay-file URI names. outbox is the service's outbox directory ("" if none).
func (r *Registry) Resolve(service, outbox, uri string) (*Entry, error) {
	rest, ok := strings.CutPrefix(uri, "toprelay-file://")
	if !ok {
		return nil, errors.New("not a toprelay-file URI")
	}
	if rel, isPath := strings.CutPrefix(rest, "/"); isPath {
		// Apps that build the URI with a URL library percent-encode names with spaces or non-ASCII characters.
		dec, err := url.PathUnescape(rel)
		if err != nil {
			return nil, errors.New("bad file path")
		}
		return resolveOutbox(service, outbox, dec)
	}
	r.mu.Lock()
	o, ok := r.offers[rest]
	r.mu.Unlock()
	if !ok || !r.Now().Before(o.expires) || o.e.Service != service {
		return nil, errors.New("the file isn't offered (it expired, or the agent restarted)")
	}
	// The identity a download must match is the file as it is now, when the link is made.
	e, err := snapshot(service, o.e.Path, o.e.Name, o.e.ContentType)
	if err != nil {
		return nil, errors.New("the offered file is gone or no longer usable")
	}
	return e, nil
}

// resolveOutbox maps a relative path into the outbox, refusing anything that leaves it (.., symlinks out).
func resolveOutbox(service, outbox, rel string) (*Entry, error) {
	if outbox == "" {
		return nil, errors.New("this service has no outbox directory (toprelay add … --files-dir DIR)")
	}
	if rel == "" || strings.Contains(rel, "\\") || path.Clean("/"+rel) != "/"+rel {
		return nil, errors.New("bad file path")
	}
	// Errors here reach the bot, so they never include a local path.
	root, err := filepath.EvalSymlinks(outbox)
	if err != nil {
		return nil, errors.New("the service's outbox directory is missing or unreadable")
	}
	full, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errors.New("no such file in the outbox")
		}
		return nil, errors.New("the file in the outbox can't be read")
	}
	if !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return nil, errors.New("the path leaves the outbox")
	}
	e, err := snapshot(service, full, path.Base(rel), "")
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			return nil, errors.New("the file in the outbox can't be read")
		}
		return nil, err
	}
	return e, nil
}

// CheckOutbox checks an outbox directory: absolute, yours, and not writable by group or others.
func CheckOutbox(dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("the files directory must be an absolute path")
	}
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	_, _, uid, ok := identity(st)
	if !ok || uid != os.Getuid() {
		return fmt.Errorf("%s is not owned by you", dir)
	}
	if st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("other users can write to %s (mode %04o); chmod go-w it", dir, st.Mode().Perm())
	}
	return nil
}

// NewDownload makes a single-use download id for an entry, for one client.
func (r *Registry) NewDownload(e *Entry, client string, ttl time.Duration) (string, time.Time, error) {
	if ttl <= 0 || ttl > keys.MaxFileURLTTL {
		ttl = DefaultLinkTTL
	}
	id, err := keys.NewFileID()
	if err != nil {
		return "", time.Time{}, err
	}
	now := r.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(now)
	if len(r.downloads) >= MaxDownloads {
		return "", time.Time{}, errors.New("too many outstanding download links")
	}
	exp := now.Add(ttl).Truncate(time.Second)
	r.downloads[id] = download{e: e, client: client, expires: exp}
	return id, exp, nil
}

// Take returns a download's entry and forgets the id, so a link works once.
func (r *Registry) Take(service, id string) (*Entry, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.downloads[id]
	if !ok || d.e.Service != service {
		return nil, "", errors.New("unknown or used download link")
	}
	delete(r.downloads, id)
	if !r.Now().Before(d.expires) {
		return nil, "", errors.New("the download link expired")
	}
	return d.e, d.client, nil
}

// contentDisposition is attachment with an ASCII fallback and an RFC 5987 UTF-8 name.
func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, pathEscape(name))
}

func pathEscape(s string) string {
	const hexd = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hexd[c>>4])
			b.WriteByte(hexd[c&15])
		}
	}
	return b.String()
}

// Serve streams an entry, refusing it if the file changed since it was linked (another file renamed into its
// place, or its content rewritten).
func Serve(w http.ResponseWriter, e *Entry) {
	f, err := os.Open(e.Path)
	if err != nil {
		http.Error(w, "toprelay: the file is no longer there.", http.StatusGone)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	same := false
	if err == nil {
		dev, ino, _, ok := identity(st)
		same = ok && dev == e.dev && ino == e.ino && st.Size() == e.Size && st.ModTime().Equal(e.mtime)
	}
	if !same {
		http.Error(w, "toprelay: the file changed after it was shared; ask for it again.", http.StatusGone)
		return
	}
	w.Header().Set("Content-Type", e.ContentType)
	w.Header().Set("Content-Disposition", contentDisposition(e.Name))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(e.Size, 10))
	w.WriteHeader(http.StatusOK)
	io.Copy(w, io.LimitReader(f, e.Size))
}
