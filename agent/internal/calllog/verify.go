package calllog

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Problem is one place where the log doesn't check out.
type Problem struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Seq  uint64 `json:"seq,omitempty"`
	What string `json:"what"`
}

// VerifyReport is the result of checking a service's call log.
type VerifyReport struct {
	OK bool `json:"ok"`
	// Lines are all lines read; Calls the call records among them; Legacy the lines an older agent wrote
	// without a chain (they can't be checked).
	Lines  int `json:"lines"`
	Calls  int `json:"calls"`
	Legacy int `json:"legacy"`
	// FirstSeq and LastSeq bound the chain that was checked. A first line with seq above 1 means older lines
	// were rotated out or pruned (pruning is recorded in the chain).
	FirstSeq uint64 `json:"firstSeq"`
	LastSeq  uint64 `json:"lastSeq"`
	// Checkpoints are the valid signed checkpoints; SignedThrough is the seq of the last one. Unsigned counts
	// the lines after it (they become signed at the next checkpoint).
	Checkpoints   int    `json:"checkpoints"`
	SignedThrough uint64 `json:"signedThrough"`
	Unsigned      int    `json:"unsigned"`
	// Prunes are retention records; Pruned the records they say were removed.
	Prunes   int       `json:"prunes"`
	Pruned   int       `json:"pruned"`
	KeyID    string    `json:"keyId,omitempty"`
	Problems []Problem `json:"problems"`
}

// Verify checks a call log (the rotated file, then the current one): every chained line must follow the one
// before it (seq +1, prev = its SHA-256), and every checkpoint must carry a valid signature by pub for service.
// With pub nil, only the chain is checked and checkpoints are counted as unverified problems.
func Verify(path, service string, pub ed25519.PublicKey) (VerifyReport, error) {
	rep := VerifyReport{Problems: []Problem{}}
	var wantKey string
	if pub != nil {
		wantKey = KeyID(pub)
		rep.KeyID = wantKey
	}
	var prevHash string
	var prevSeq uint64
	chained := false
	sinceCkpt := 0
	problem := func(file string, line int, seq uint64, format string, a ...any) {
		rep.Problems = append(rep.Problems, Problem{File: filepath.Base(file), Line: line, Seq: seq, What: fmt.Sprintf(format, a...)})
	}
	for _, p := range []string{path + ".1", path} {
		f, err := os.Open(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return rep, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		n := 0
		for sc.Scan() {
			n++
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			rep.Lines++
			h := lineHash(line)
			var r Record
			if err := json.Unmarshal(line, &r); err != nil {
				problem(p, n, 0, "not a log record (damaged or edited): %v", err)
				prevHash, chained = h, false
				continue
			}
			if r.Seq == 0 {
				if chained {
					problem(p, n, 0, "a line without a sequence number inside the chain (inserted)")
				} else {
					rep.Legacy++
				}
				prevHash = h
				continue
			}
			switch {
			case !chained:
				// The first chained line anchors the check. Its prev is checkable only when the line before it
				// is still here (an older agent's last line); otherwise earlier lines were rotated out or pruned.
				if prevHash != "" && r.Prev != prevHash {
					problem(p, n, r.Seq, "doesn't follow the line before it")
				}
				if rep.FirstSeq == 0 {
					rep.FirstSeq = r.Seq
				}
			case r.Seq != prevSeq+1:
				problem(p, n, r.Seq, "sequence jumps from %d to %d (lines removed or reordered)", prevSeq, r.Seq)
			case r.Prev != prevHash:
				problem(p, n, r.Seq, "the line before it was changed")
			}
			chained = true
			prevSeq, prevHash = r.Seq, h
			rep.LastSeq = r.Seq
			sinceCkpt++
			switch r.Type {
			case "":
				rep.Calls++
			case TypePrune:
				rep.Prunes++
				rep.Pruned += r.Removed
			case TypeCheckpoint:
				switch {
				case pub == nil:
					problem(p, n, r.Seq, "checkpoint not verified (no public key given)")
				case r.KeyID != wantKey:
					problem(p, n, r.Seq, "checkpoint signed by another key (%s)", r.KeyID)
				default:
					sig, err := base64.RawURLEncoding.DecodeString(r.Sig)
					if err != nil || !ed25519.Verify(pub, CheckpointMessage(service, r.Seq, r.Prev, r.Time), sig) {
						problem(p, n, r.Seq, "checkpoint signature is invalid")
						continue
					}
					rep.Checkpoints++
					rep.SignedThrough = r.Seq
					sinceCkpt = 0
				}
			default:
				problem(p, n, r.Seq, "unknown line type %q", r.Type)
			}
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return rep, err
		}
	}
	rep.Unsigned = sinceCkpt
	if rep.SignedThrough == 0 {
		rep.Unsigned = int(max(0, int64(rep.LastSeq)-int64(rep.FirstSeq)+1))
		if rep.LastSeq == 0 {
			rep.Unsigned = 0
		}
	}
	rep.OK = len(rep.Problems) == 0
	return rep, nil
}
