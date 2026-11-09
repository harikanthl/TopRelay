package shield

import (
	"encoding/base64"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Finding is one suspicious passage: the rule that matched and a short excerpt (normalized, so hidden or
// disguised text is shown as the model would read it).
type Finding struct {
	Rule    string `json:"rule"`
	Excerpt string `json:"excerpt,omitempty"`
}

// Injection rules. Phrase rules run on normalized text: invisible characters removed, look-alike letters folded
// to ASCII, lower case, single spaces.
const (
	RuleHiddenUnicode = "hidden-unicode" // Unicode tag characters: text the user can't see but the model reads
	RuleBidiControl   = "bidi-control"   // direction overrides that make text display differently than it reads
)

type phraseRule struct {
	name string
	re   *regexp.Regexp
}

var phraseRules = []phraseRule{
	{"ignore-instructions", regexp.MustCompile(`\b(?:ignore|disregard|forget|override|bypass)\s+(?:all\s+|any\s+|every\s+)?(?:of\s+)?(?:the\s+|your\s+|my\s+)?(?:previous|prior|above|earlier|preceding|system|original|safety)\s+(?:instructions?|prompts?|rules|directions|guidelines|context|messages?)`)},
	{"role-override", regexp.MustCompile(`\byou\s+are\s+now\s+(?:an?\s+)?(?:unrestricted|jailbroken|developer\s+mode|dan|evil|different)\b|\bnew\s+system\s+prompt\b|\b(?:begin|start)\s+system\s+(?:prompt|message)\b|\bsystem\s*override\b`)},
	{"conceal-from-user", regexp.MustCompile(`\b(?:do\s+not|don't|dont|never|without)\s+(?:tell(?:ing)?|inform(?:ing)?|mention(?:ing)?|reveal(?:ing)?|show(?:ing)?|alert(?:ing)?|notify(?:ing)?|let(?:ting)?)\s+(?:this\s+(?:to\s+)?)?(?:the\s+)?user\b|\bthe\s+user\s+(?:must|should)\s+not\s+(?:know|see|be\s+told)\b`)},
	{"instruction-tag", regexp.MustCompile(`<\s*/?\s*(?:important|system|instructions?|secret|hidden|admin)\s*>`)},
	{"sensitive-file", regexp.MustCompile(`(?:~|\$home|/users/[^/\s]+|/home/[^/\s]+|%userprofile%)/\.(?:ssh|aws|gnupg|kube|docker|netrc|git-credentials)\b|\bid_(?:rsa|ed25519|ecdsa)\b|\.aws/credentials\b|\bclaude_desktop_config\.json\b|\.cursor/mcp\.json\b`)},
	{"exfiltration", regexp.MustCompile(`\b(?:send|post|upload|forward|transmit|exfiltrate|leak|copy)\b[^.\n]{0,80}?\b(?:to|at|into)\s+(?:https?://|ftp://|\S+@\S+\.\w)`)},
	{"tool-shadowing", regexp.MustCompile(`\b(?:when|whenever|before|after|instead\s+of)\s+(?:using|calling|invoking|running)\s+(?:the\s+)?(?:(?:other|any|another|every|all)\s+){1,2}tools?\b|\binstead\s+of\s+(?:calling|using)\s+the\s+\S+\s+tool\b`)},
}

// InjectionRules returns the names of the built-in injection rules.
func InjectionRules() []string {
	out := []string{RuleHiddenUnicode, RuleBidiControl}
	for _, r := range phraseRules {
		out = append(out, r.name, r.name+"(base64)")
	}
	return out
}

// homoglyphs folds letters that look like ASCII (Cyrillic, Greek, Latin variants) to the ASCII they imitate.
var homoglyphs = map[rune]rune{
	'а': 'a', 'е': 'e', 'о': 'o', 'р': 'p', 'с': 'c', 'у': 'y', 'х': 'x', 'і': 'i', 'ј': 'j', 'ѕ': 's', 'ԁ': 'd',
	'ɡ': 'g', 'һ': 'h', 'ӏ': 'l', 'ո': 'n', 'ս': 'u', 'ԝ': 'w', 'к': 'k', 'м': 'm', 'т': 't', 'в': 'b', 'н': 'h',
	'А': 'a', 'В': 'b', 'Е': 'e', 'К': 'k', 'М': 'm', 'Н': 'h', 'О': 'o', 'Р': 'p', 'С': 'c', 'Т': 't', 'Х': 'x',
	'Ү': 'y', 'І': 'i', 'Ј': 'j', 'Ѕ': 's',
	'α': 'a', 'ε': 'e', 'ι': 'i', 'ο': 'o', 'ρ': 'p', 'τ': 't', 'υ': 'u', 'ν': 'v', 'κ': 'k', 'χ': 'x', 'ω': 'w',
	'Α': 'a', 'Β': 'b', 'Ε': 'e', 'Ζ': 'z', 'Η': 'h', 'Ι': 'i', 'Κ': 'k', 'Μ': 'm', 'Ν': 'n', 'Ο': 'o', 'Ρ': 'p',
	'Τ': 't', 'Υ': 'y', 'Χ': 'x',
	'ı': 'i', 'ℓ': 'l', '‘': '\'', '’': '\'', 'ʼ': '\'',
}

func isTag(r rune) bool { return r >= 0xE0000 && r <= 0xE007F }

func isBidi(r rune) bool { return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) }

// isInvisible reports format characters that render as nothing (zero-width spaces and joiners, word joiner,
// BOM, soft hyphen, variation selectors).
func isInvisible(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F, r >= 0x2060 && r <= 0x2064, r == 0xFEFF, r == 0x00AD, r == 0x180E,
		r >= 0xFE00 && r <= 0xFE0F:
		return true
	}
	return false
}

// normalize renders text the way a model effectively reads it, for matching.
func normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if isTag(r) || isBidi(r) || isInvisible(r) {
			continue
		}
		if r >= 0xFF01 && r <= 0xFF5E { // fullwidth ASCII
			r -= 0xFEE0
		}
		if h, ok := homoglyphs[r]; ok {
			r = h
		}
		if unicode.IsSpace(r) {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(unicode.ToLower(r))
	}
	return strings.TrimSpace(b.String())
}

// hiddenTagText decodes Unicode tag characters (U+E0020–E007E mirror ASCII) into the text they smuggle.
func hiddenTagText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0xE0020 && r <= 0xE007E {
			b.WriteRune(r - 0xE0000)
		}
	}
	return b.String()
}

var base64Run = regexp.MustCompile(`[A-Za-z0-9+/_\-]{32,}={0,2}`)

// decodedRuns returns the readable text hidden in base64 runs of s.
func decodedRuns(s string) []string {
	var out []string
	for _, run := range base64Run.FindAllString(s, 16) {
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			b, err := enc.DecodeString(run)
			if err != nil || !utf8.Valid(b) {
				continue
			}
			printable := 0
			for _, r := range string(b) {
				if unicode.IsPrint(r) || unicode.IsSpace(r) {
					printable++
				}
			}
			if printable*10 >= utf8.RuneCount(b)*9 {
				out = append(out, string(b))
			}
			break
		}
	}
	return out
}

func excerpt(s string, start, end int) string {
	const pad = 24
	a, z := max(0, start-pad), min(len(s), end+pad)
	for a > 0 && !utf8.RuneStart(s[a]) {
		a--
	}
	for z < len(s) && !utf8.RuneStart(s[z]) {
		z++
	}
	e := s[a:z]
	if a > 0 {
		e = "…" + e
	}
	if z < len(s) {
		e += "…"
	}
	return e
}

func scanPhrases(norm, suffix string, add func(Finding)) {
	for _, r := range phraseRules {
		if m := r.re.FindStringIndex(norm); m != nil {
			add(Finding{Rule: r.name + suffix, Excerpt: excerpt(norm, m[0], m[1])})
		}
	}
}

// ScanText looks for text meant to steer the model: invisible or direction-changing characters, and
// instruction phrases (also inside smuggled tag text and base64). Each rule is reported once.
func ScanText(s string) []Finding {
	var out []Finding
	seen := map[string]bool{}
	add := func(f Finding) {
		if !seen[f.Rule] {
			seen[f.Rule] = true
			out = append(out, f)
		}
	}
	hasTag, hasBidi := false, false
	for _, r := range s {
		hasTag = hasTag || isTag(r)
		hasBidi = hasBidi || isBidi(r)
	}
	if hasTag {
		hidden := hiddenTagText(s)
		add(Finding{Rule: RuleHiddenUnicode, Excerpt: excerpt(hidden, 0, min(len(hidden), 56))})
		scanPhrases(normalize(hidden), "", add)
	}
	if hasBidi {
		add(Finding{Rule: RuleBidiControl})
	}
	scanPhrases(normalize(s), "", add)
	for _, d := range decodedRuns(s) {
		scanPhrases(normalize(d), "(base64)", add)
	}
	return out
}

// ScanJSON runs ScanText on every string value of a JSON document; each rule is reported once.
func ScanJSON(raw []byte) []Finding {
	v, err := decode(raw)
	if err != nil {
		return nil
	}
	var out []Finding
	seen := map[string]bool{}
	eachString(v, func(s string) {
		for _, f := range ScanText(s) {
			if !seen[f.Rule] {
				seen[f.Rule] = true
				out = append(out, f)
			}
		}
	})
	return out
}

// Rules returns the rule names of findings.
func Rules(fs []Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Rule
	}
	return out
}
