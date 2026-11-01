package keys

import (
	"strings"
	"testing"
	"time"
)

const (
	agentRoot  = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	bearerRoot = "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8"
	vecBearer  = "trk1.eyJpIjoiMDEyMzQ1Njc4OWFiY2RlZiIsImMiOlsic3ZjPW5vdGVzIiwiZz0xIl19.ByA-aoTDoDDxKP2uBxEVX5f_pNZRme7o3HaKWh6kn3I"
	vecNarrow  = "trk1.eyJpIjoiMDEyMzQ1Njc4OWFiY2RlZiIsImMiOlsic3ZjPW5vdGVzIiwiZz0xIiwidG9vbHM9c2VhcmNoLHJlYWQiLCJleHA9NDEwMjQ0NDgwMCJdfQ.x26sPAObXFuxuW6XUmyku29lI8HiecbJgLrwd32DRxg"
)

func TestRootVectors(t *testing.T) {
	a, err := DecodeRoot(agentRoot)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range a {
		if int(b) != i {
			t.Fatalf("agent root byte %d = %d", i, b)
		}
	}
	b, err := DecodeRoot(bearerRoot)
	if err != nil {
		t.Fatal(err)
	}
	if b[0] != 0x20 || b[31] != 0x3f {
		t.Fatal("bearer root bytes wrong")
	}
}

func TestAgentKeyVector(t *testing.T) {
	got, err := AgentKey(agentRoot, "notes", 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := "tra1.TA3-AhWB6Ka9MRN31Tk1D5C0kgDAgB4eoiJpZWwq7Aw"; got != want {
		t.Fatalf("agent key = %s, want %s", got, want)
	}
	other, _ := AgentKey(agentRoot, "notes", 2)
	if other == got {
		t.Fatal("gen must change the key")
	}
}

func TestBearerVectors(t *testing.T) {
	b, err := Mint(bearerRoot, "0123456789abcdef", []string{"svc=notes", "g=1"})
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != vecBearer {
		t.Fatalf("mint = %s\nwant   %s", b, vecBearer)
	}
	n := Narrow(b, "tools=search,read", "exp=4102444800")
	if n.String() != vecNarrow {
		t.Fatalf("narrow = %s\nwant     %s", n, vecNarrow)
	}
	// Narrowing must not modify the original.
	if b.String() != vecBearer {
		t.Fatal("Narrow modified its input")
	}
	// Narrowing a parsed key gives the same result.
	p, err := Parse(vecBearer)
	if err != nil {
		t.Fatal(err)
	}
	if Narrow(p, "tools=search,read", "exp=4102444800").String() != vecNarrow {
		t.Fatal("narrowing a parsed key differs")
	}
}

func TestVerify(t *testing.T) {
	now := time.Unix(1793500000, 0)
	p, err := Parse(vecNarrow)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := Verify(bearerRoot, p, Request{Service: "notes", Gen: 1, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(tools, ",") != "search,read" {
		t.Fatalf("tools = %v", tools)
	}
	if _, err := Verify(bearerRoot, p, Request{Service: "other", Gen: 1, Now: now}); err == nil {
		t.Fatal("wrong service accepted")
	}
	if _, err := Verify(bearerRoot, p, Request{Service: "notes", Gen: 2, Now: now}); err == nil {
		t.Fatal("wrong gen accepted")
	}
	if _, err := Verify(bearerRoot, p, Request{Service: "notes", Gen: 1, Now: time.Unix(4102444800, 0)}); err == nil {
		t.Fatal("expired key accepted")
	}
	// Tamper: change a caveat without re-signing.
	bad := p
	bad.Caveats = []string{"svc=notes", "g=1", "tools=search,read,write", "exp=4102444800"}
	if _, err := Verify(bearerRoot, bad, Request{Service: "notes", Gen: 1, Now: now}); err == nil {
		t.Fatal("tampered key accepted")
	}
	// Unknown caveats fail closed.
	u := Narrow(p, "ip=1.2.3.4")
	if _, err := Verify(bearerRoot, u, Request{Service: "notes", Gen: 1, Now: now}); err == nil {
		t.Fatal("unknown caveat accepted")
	}
	// Intersection of tools caveats.
	i := Narrow(p, "tools=read,write")
	tools, err = Verify(bearerRoot, i, Request{Service: "notes", Gen: 1, Now: now})
	if err != nil || strings.Join(tools, ",") != "read" {
		t.Fatalf("intersection = %v, %v", tools, err)
	}
	e := Narrow(p, "tools=write")
	tools, err = Verify(bearerRoot, e, Request{Service: "notes", Gen: 1, Now: now})
	if err != nil || tools == nil || len(tools) != 0 {
		t.Fatalf("empty intersection = %#v, %v", tools, err)
	}
	// No tools caveat: nil restriction.
	plain, _ := Parse(vecBearer)
	tools, err = Verify(bearerRoot, plain, Request{Service: "notes", Gen: 1, Now: now})
	if err != nil || tools != nil {
		t.Fatalf("unrestricted = %#v, %v", tools, err)
	}
}

func TestParseRejects(t *testing.T) {
	for _, s := range []string{"", "tra1.x", "trk1.abc", "trk1.!!.xx", "trk1.e30." + strings.Repeat("A", 43)} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted", s)
		}
	}
}

func TestMintChecks(t *testing.T) {
	if _, err := Mint(bearerRoot, "XYZ", nil); err == nil {
		t.Fatal("bad id accepted")
	}
	if _, err := Mint("short", "0123456789abcdef", nil); err == nil {
		t.Fatal("bad root accepted")
	}
}

func TestNewRootAndID(t *testing.T) {
	r, err := NewRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeRoot(r); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(r, "+/=") {
		t.Fatal("root must be base64url without padding")
	}
	id, err := NewKeyID()
	if err != nil || !keyIDRe.MatchString(id) {
		t.Fatalf("id %q %v", id, err)
	}
}

func TestCaveatsAndDescribe(t *testing.T) {
	exp := time.Unix(4102444800, 0)
	c := Caveats("notes", 3, []string{"a", "b"}, exp)
	want := "svc=notes g=3 tools=a,b exp=4102444800"
	if strings.Join(c, " ") != want {
		t.Fatalf("caveats = %v", c)
	}
	b, _ := Mint(bearerRoot, "0123456789abcdef", c)
	b = Narrow(b, "tools=b,c", "exp=4102444000")
	d := Describe(b)
	if d.Service != "notes" || d.Gen != "3" || strings.Join(d.Tools, ",") != "b" || d.Expires.Unix() != 4102444000 {
		t.Fatalf("describe = %+v", d)
	}
	if got := Caveats("x", 1, nil, time.Time{}); len(got) != 2 {
		t.Fatalf("minimal caveats = %v", got)
	}
}
