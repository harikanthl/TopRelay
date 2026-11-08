package mcpproxy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
)

// FilterSSE copies an SSE stream from r to w event by event, rewriting events whose data is a response to a
// tools/list id. Other events pass through byte for byte. It flushes after every event. onSeen receives the tool
// names seen in each filtered result.
func FilterSSE(w io.Writer, r io.Reader, listIDs map[string]bool, allowed Allowed, onSeen func([]string)) error {
	return TransformSSE(w, r, ListTransform(listIDs, allowed), onSeen)
}

// TransformSSE copies an SSE stream event by event, applying t to each event's data. Unchanged events pass
// through byte for byte.
func TransformSSE(w io.Writer, r io.Reader, t Transform, onSeen func([]string)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	fl, _ := w.(http.Flusher)
	var event [][]byte
	flush := func() error {
		if len(event) == 0 {
			return nil
		}
		out := rewriteEvent(event, t, onSeen)
		event = event[:0]
		if _, err := w.Write(out); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			event = append(event, line)
			if len(bytes.TrimRight(line, "\r\n")) == 0 && line[len(line)-1] == '\n' {
				if ferr := flush(); ferr != nil {
					return ferr
				}
			}
		}
		if err != nil {
			if ferr := flush(); ferr != nil {
				return ferr
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func rewriteEvent(lines [][]byte, t Transform, onSeen func([]string)) []byte {
	raw := bytes.Join(lines, nil)
	if t == nil {
		return raw
	}
	var data []string
	for _, l := range lines {
		s := strings.TrimRight(string(l), "\r\n")
		if s == "data" {
			data = append(data, "")
		} else if v, ok := strings.CutPrefix(s, "data:"); ok {
			data = append(data, strings.TrimPrefix(v, " "))
		}
	}
	if len(data) == 0 {
		return raw
	}
	payload := []byte(strings.Join(data, "\n"))
	out, changed, seen := t(payload)
	if onSeen != nil && len(seen) > 0 {
		onSeen(seen)
	}
	if !changed {
		return raw
	}
	var b bytes.Buffer
	wroteData := false
	for _, l := range lines {
		s := strings.TrimRight(string(l), "\r\n")
		isData := s == "data" || strings.HasPrefix(s, "data:")
		switch {
		case isData && !wroteData:
			b.WriteString("data: ")
			b.Write(out)
			b.WriteByte('\n')
			wroteData = true
		case isData:
		default:
			b.Write(l)
		}
	}
	return b.Bytes()
}
