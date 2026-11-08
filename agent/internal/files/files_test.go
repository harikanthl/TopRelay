package files

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOutboxResolve(t *testing.T) {
	root := t.TempDir()
	box := filepath.Join(root, "outbox")
	os.Mkdir(box, 0o700)
	os.Mkdir(filepath.Join(box, "sub"), 0o700)
	write(t, filepath.Join(box, "report.pdf"), "%PDF-1.7")
	write(t, filepath.Join(box, "sub", "a b.txt"), "hi")
	write(t, filepath.Join(root, "secret"), "no")
	os.Symlink(filepath.Join(root, "secret"), filepath.Join(box, "escape"))
	os.Symlink(filepath.Join(box, "report.pdf"), filepath.Join(box, "alias.pdf"))
	r := NewRegistry()

	e, err := r.Resolve("notes", box, "toprelay-file:///report.pdf")
	if err != nil || e.Name != "report.pdf" || e.ContentType != "application/pdf" || e.Size != 8 {
		t.Fatalf("%+v %v", e, err)
	}
	if e, err := r.Resolve("notes", box, "toprelay-file:///sub/a b.txt"); err != nil || e.Name != "a b.txt" {
		t.Fatalf("subdir: %+v %v", e, err)
	}
	if _, err := r.Resolve("notes", box, "toprelay-file:///alias.pdf"); err != nil {
		t.Fatalf("a symlink inside the outbox: %v", err)
	}
	for _, bad := range []string{"toprelay-file:///../secret", "toprelay-file:///escape", "toprelay-file:///sub/../../secret",
		"toprelay-file:///", "toprelay-file:///sub", "toprelay-file:///missing", "toprelay-file:///a\\b", "toprelay-file:////etc/passwd",
		"file:///etc/passwd"} {
		if _, err := r.Resolve("notes", box, bad); err == nil {
			t.Errorf("%q resolved", bad)
		}
	}
	if _, err := r.Resolve("notes", "", "toprelay-file:///report.pdf"); err == nil || !strings.Contains(err.Error(), "--files-dir") {
		t.Fatalf("no outbox: %v", err)
	}

	if err := CheckOutbox(box); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(root, "open")
	os.Mkdir(open, 0o777)
	os.Chmod(open, 0o777)
	if CheckOutbox(open) == nil || CheckOutbox("relative") == nil || CheckOutbox(filepath.Join(box, "report.pdf")) == nil {
		t.Fatal("bad outbox accepted")
	}
}

func TestOffersAndSingleUseDownloads(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "scan.png")
	write(t, p, "PNGDATA")
	now := time.Unix(1793500000, 0)
	r := NewRegistry()
	r.Now = func() time.Time { return now }

	id, exp, err := r.Offer("notes", p, "Scan 1.png", "", 0)
	if err != nil || !exp.Equal(now.Add(DefaultOfferTTL)) {
		t.Fatal(id, exp, err)
	}
	e, err := r.Resolve("notes", "", "toprelay-file://"+id)
	if err != nil || e.Name != "Scan 1.png" || e.ContentType != "image/png" {
		t.Fatalf("%+v %v", e, err)
	}
	if _, err := r.Resolve("other", "", "toprelay-file://"+id); err == nil {
		t.Fatal("another service resolved the offer")
	}
	if _, _, err := r.Offer("notes", "relative.png", "", "", 0); err == nil {
		t.Fatal("relative path offered")
	}
	if _, _, err := r.Offer("notes", dir, "", "", 0); err == nil {
		t.Fatal("directory offered")
	}

	fid, fexp, err := r.NewDownload(e, "key:0123456789abcdef", 0)
	if err != nil || !fexp.Equal(now.Add(DefaultLinkTTL)) {
		t.Fatal(err)
	}
	if _, _, err := r.Take("other", fid); err == nil {
		t.Fatal("another service took the download")
	}
	got, client, err := r.Take("notes", fid)
	if err != nil || got != e || client != "key:0123456789abcdef" {
		t.Fatal(err)
	}
	if _, _, err := r.Take("notes", fid); err == nil {
		t.Fatal("a link worked twice")
	}

	// Expiry: links and offers.
	fid2, _, _ := r.NewDownload(e, "c", time.Minute)
	now = now.Add(2 * time.Minute)
	if _, _, err := r.Take("notes", fid2); err == nil {
		t.Fatal("expired link worked")
	}
	now = now.Add(DefaultOfferTTL)
	if _, err := r.Resolve("notes", "", "toprelay-file://"+id); err == nil {
		t.Fatal("expired offer resolved")
	}
}

func TestServe(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "résumé \"final\".pdf")
	write(t, p, "%PDF-data")
	e, err := snapshot("notes", p, "", "")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	Serve(w, e)
	if w.Code != 200 || w.Body.String() != "%PDF-data" || w.Header().Get("Content-Type") != "application/pdf" ||
		w.Header().Get("Content-Disposition") != `attachment; filename="r_sum_ _final_.pdf"; filename*=UTF-8''r%C3%A9sum%C3%A9%20%22final%22.pdf` {
		t.Fatalf("%d %v %q", w.Code, w.Header(), w.Body)
	}
	// Rewritten after it was linked: refused. Replaced by another file: refused.
	time.Sleep(10 * time.Millisecond)
	write(t, p, "%PDF-evil")
	w = httptest.NewRecorder()
	Serve(w, e)
	if w.Code != 410 {
		t.Fatalf("changed file served: %d", w.Code)
	}
	e2, _ := snapshot("notes", p, "", "")
	os.Remove(p)
	write(t, p, "%PDF-evil")
	w = httptest.NewRecorder()
	Serve(w, e2)
	if w.Code != 410 {
		t.Fatalf("replaced file served: %d", w.Code)
	}
}
