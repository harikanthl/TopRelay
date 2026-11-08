package keys

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxFileURLTTL is the longest a signed file URL is valid.
const MaxFileURLTTL = 15 * time.Minute

// FileIDRE matches a download id: 32 lowercase hex characters.
var FileIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// NewFileID returns a random download id.
func NewFileID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// FileSig signs one download (docs/protocol.md "Files"):
//
//	file_key = HMAC-SHA256(b64url_decode(BEARER_ROOT), "toprelay/files/v1")
//	sig      = b64url(HMAC-SHA256(file_key, "toprelay/file/v1/" + service + "/" + gen + "/" + id + "/" + exp))
func FileSig(bearerRoot, service string, gen int, id string, exp int64) (string, error) {
	root, err := DecodeRoot(bearerRoot)
	if err != nil {
		return "", err
	}
	fk := mac(root, "toprelay/files/v1")
	return base64.RawURLEncoding.EncodeToString(mac(fk, fmt.Sprintf("toprelay/file/v1/%s/%d/%s/%d", service, gen, id, exp))), nil
}

// FileURL is the public, single-use URL of a download.
func FileURL(publicURL, bearerRoot, service string, gen int, id string, exp time.Time) (string, error) {
	if !FileIDRE.MatchString(id) {
		return "", fmt.Errorf("bad file id %q", id)
	}
	sig, err := FileSig(bearerRoot, service, gen, id, exp.Unix())
	if err != nil {
		return "", err
	}
	return strings.TrimRight(publicURL, "/") + "/" + service + "/files/" + id + "?e=" + strconv.FormatInt(exp.Unix(), 10) + "&s=" + sig, nil
}
