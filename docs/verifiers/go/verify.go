// Verifies X-TopRelay-Caller (docs/protocol.md "Agent → local app"). Standard library only.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// BEGIN verifier
// VerifyTopRelayCaller returns the caller's client id and true if the header is valid. target is r.RequestURI.
func VerifyTopRelayCaller(key []byte, header, method, target string, body []byte, now time.Time) (string, bool) {
	parts := strings.Split(header, ";")
	f := map[string]string{}
	for _, p := range parts[1:] {
		if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok {
			f[k] = v
		}
	}
	ts, err := strconv.ParseInt(f["ts"], 10, 64)
	if strings.TrimSpace(parts[0]) != "v1" || err != nil || strings.Trim(f["ts"], "0123456789") != "" || now.Sub(time.Unix(ts, 0)).Abs() > time.Minute {
		return "", false
	}
	sum := sha256.Sum256(body)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(strings.Join([]string{"toprelay-caller-v1", method, target, f["client"], f["ts"], f["nonce"], hex.EncodeToString(sum[:])}, "\n")))
	if !hmac.Equal([]byte(hex.EncodeToString(m.Sum(nil))), []byte(f["sig"])) {
		return "", false
	}
	client, err := url.PathUnescape(f["client"])
	return client, err == nil
}
// END verifier
func main() { // the protocol.md test vector
	a := os.Args[1:]
	now, _ := strconv.ParseInt(a[5], 10, 64)
	if c, ok := VerifyTopRelayCaller([]byte(a[0]), a[1], a[2], a[3], []byte(a[4]), time.Unix(now, 0)); ok {
		fmt.Println(c)
	} else {
		fmt.Println("None")
	}
}
