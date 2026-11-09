// Package shield inspects what a local app sends back before it leaves the computer: it redacts credentials
// (leaks) and flags text that tries to instruct the model (injection). It works on decoded JSON strings, so JSON
// escaping can't hide a match.
package shield

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/harikanthl/toprelay/agent/internal/redact"
)

// Leak modes.
const (
	LeaksRedact = "redact" // replace each secret with [REDACTED:<rule>] (the default)
	LeaksBlock  = "block"  // replace the whole tool result with an error
	LeaksOff    = "off"
)

// Injection modes.
const (
	InjectionWarn = "warn" // log and report, never change the content (the default)
	InjectionOff  = "off"
)

// leakRule is one credential shape. group > 0 redacts only that submatch (the secret part of "key=secret").
type leakRule struct {
	name  string
	re    *regexp.Regexp
	group int
}

// leakRules are credential formats with a distinctive prefix or structure, so ordinary text doesn't match.
var leakRules = []leakRule{
	{name: "private-key", re: regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----(?:[\s\S]*?-----END (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----|[A-Za-z0-9+/=\s]*)`)},
	{name: "aws-access-key", re: regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}\b`)},
	{name: "aws-secret-key", re: regexp.MustCompile(`(?i)aws.{0,20}?(?:secret|private).{0,20}?["'=:\s]+([A-Za-z0-9/+]{40})(?:[^A-Za-z0-9/+=]|$)`), group: 1},
	{name: "github-token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{50,255})\b`)},
	{name: "gitlab-token", re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,}`)},
	{name: "slack-token", re: regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9\-]{10,}`)},
	{name: "slack-webhook", re: regexp.MustCompile(`https://hooks\.slack\.com/services/T[A-Z0-9]+/B[A-Z0-9]+/[A-Za-z0-9]+`)},
	{name: "stripe-key", re: regexp.MustCompile(`\b(?:sk|rk)_live_[A-Za-z0-9]{20,}\b|\bwhsec_[A-Za-z0-9]{24,}\b`)},
	{name: "google-api-key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}`)},
	{name: "anthropic-key", re: regexp.MustCompile(`\bsk-ant-(?:api|admin)[0-9]{2}-[A-Za-z0-9_\-]{80,}`)},
	{name: "openai-key", re: regexp.MustCompile(`\bsk-(?:proj|svcacct|admin)-[A-Za-z0-9_\-]{20,}|\bsk-[A-Za-z0-9]{20}T3BlbkFJ[A-Za-z0-9]{20}\b`)},
	{name: "huggingface-token", re: regexp.MustCompile(`\bhf_[A-Za-z0-9]{34,}\b`)},
	{name: "npm-token", re: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`)},
	{name: "pypi-token", re: regexp.MustCompile(`\bpypi-AgEIcHlwaS5vcmc[A-Za-z0-9_\-]{50,}`)},
	{name: "sendgrid-key", re: regexp.MustCompile(`\bSG\.[A-Za-z0-9_\-]{22}\.[A-Za-z0-9_\-]{43}\b`)},
	{name: "twilio-key", re: regexp.MustCompile(`\bSK[0-9a-f]{32}\b`)},
	{name: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`)},
	{name: "url-password", re: regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.\-]{1,20}://[^\s:/@"']{1,64}:([^\s@/"']{3,128})@`), group: 1},
	{name: "azure-storage-key", re: regexp.MustCompile(`AccountKey=([A-Za-z0-9+/=]{80,})`), group: 1},
	{name: "toprelay-key", re: regexp.MustCompile(`\btr[ak]1\.[A-Za-z0-9_\-]{16,}(?:\.[A-Za-z0-9_\-]+)?`)},
}

// RuleKnownSecret names a match on a value TopRelay itself holds (the app's credential, keys, tokens).
const RuleKnownSecret = "known-secret"

// LeakRules returns the names of the built-in leak rules.
func LeakRules() []string {
	out := []string{RuleKnownSecret}
	for _, r := range leakRules {
		out = append(out, r.name)
	}
	return out
}

func redaction(rule string) string { return "[REDACTED:" + rule + "]" }

// RedactText replaces every credential in s. It returns the new text and the rules that matched (with
// repeats, one per match).
func RedactText(s string) (string, []string) {
	var hits []string
	for _, v := range redact.Values() {
		if n := strings.Count(s, v); n > 0 {
			s = strings.ReplaceAll(s, v, redaction(RuleKnownSecret))
			for range n {
				hits = append(hits, RuleKnownSecret)
			}
		}
	}
	for _, r := range leakRules {
		if !r.re.MatchString(s) {
			continue
		}
		idx := r.re.FindAllStringSubmatchIndex(s, -1)
		var b strings.Builder
		last := 0
		for _, m := range idx {
			start, end := m[0], m[1]
			if r.group > 0 {
				start, end = m[2*r.group], m[2*r.group+1]
				if start < 0 {
					continue
				}
			}
			if start < last {
				continue
			}
			b.WriteString(s[last:start])
			b.WriteString(redaction(r.name))
			last = end
			hits = append(hits, r.name)
		}
		b.WriteString(s[last:])
		s = b.String()
	}
	return s, hits
}

// mayLeak is a cheap first pass over raw JSON: false means no string in it can hold a credential. JSON escapes
// (\uXXXX, \/) could hide one from a raw scan, so their presence means the strings must be decoded.
func mayLeak(raw []byte) bool {
	if bytes.Contains(raw, []byte(`\u`)) || bytes.Contains(raw, []byte(`\/`)) {
		return true
	}
	for _, v := range redact.Values() {
		if bytes.Contains(raw, []byte(v)) {
			return true
		}
	}
	for _, r := range leakRules {
		if r.re.Match(raw) {
			return true
		}
	}
	return false
}

// RedactJSON redacts credentials in every string value of a JSON document (object keys are left alone). It
// returns the original bytes when nothing matched, so unchanged documents keep their exact encoding.
func RedactJSON(raw []byte) (out []byte, hits []string) {
	if !mayLeak(raw) {
		return raw, nil
	}
	v, err := decode(raw)
	if err != nil {
		return raw, nil
	}
	v = walkStrings(v, func(s string) string {
		ns, h := RedactText(s)
		hits = append(hits, h...)
		return ns
	})
	if len(hits) == 0 {
		return raw, nil
	}
	nb, err := encode(v)
	if err != nil {
		return raw, nil
	}
	return nb, hits
}

func decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // numbers pass through exactly
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// binaryField reports base64 binary payloads (MCP image and audio content, resource blobs): they aren't text,
// and rewriting them would corrupt the file.
func binaryField(m map[string]any, k string) bool {
	switch k {
	case "blob":
		return true
	case "data":
		t, _ := m["type"].(string)
		return t == "image" || t == "audio"
	}
	return false
}

// walkStrings rewrites every string value (not object keys) with fn.
func walkStrings(v any, fn func(string) string) any {
	switch t := v.(type) {
	case string:
		return fn(t)
	case []any:
		for i := range t {
			t[i] = walkStrings(t[i], fn)
		}
		return t
	case map[string]any:
		for k, x := range t {
			if !binaryField(t, k) {
				t[k] = walkStrings(x, fn)
			}
		}
		return t
	default:
		return v
	}
}

// eachString calls fn for every string value in a decoded JSON document.
func eachString(v any, fn func(string)) {
	switch t := v.(type) {
	case string:
		fn(t)
	case []any:
		for _, x := range t {
			eachString(x, fn)
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		slices.Sort(keys) // stable finding order
		for _, k := range keys {
			if !binaryField(t, k) {
				eachString(t[k], fn)
			}
		}
	}
}

// Summary counts rule hits: "aws-access-key", "jwt×2".
func Summary(hits []string) []string {
	counts := map[string]int{}
	var order []string
	for _, h := range hits {
		if counts[h] == 0 {
			order = append(order, h)
		}
		counts[h]++
	}
	out := make([]string, 0, len(order))
	for _, h := range order {
		if n := counts[h]; n > 1 {
			out = append(out, h+"×"+strconv.Itoa(n))
		} else {
			out = append(out, h)
		}
	}
	return out
}
