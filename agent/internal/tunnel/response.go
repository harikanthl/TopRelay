package tunnel

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// sender delivers frames to the relay in order.
type sender interface {
	send(ctx context.Context, v any) error
}

// stopper is a cancellable timer.
type stopper interface{ Stop() bool }

// afterFunc schedules f after d; tests replace it.
type afterFunc func(d time.Duration, f func()) stopper

func realAfterFunc(d time.Duration, f func()) stopper { return time.AfterFunc(d, f) }

const (
	modeBuffer = iota
	modeStream
	modeDone
)

// errCancelled is returned from Write after a cancel frame or a dropped socket.
var errCancelled = errors.New("request cancelled by the relay")

// responseWriter turns a handler's response into frames: whole when small and not streaming, otherwise a
// stream of merged chunks.
type responseWriter struct {
	ctx        context.Context
	out        sender
	id         string
	after      afterFunc
	mergeDelay time.Duration

	mu          sync.Mutex
	header      http.Header
	status      int
	wroteHeader bool
	mode        int
	buf         bytes.Buffer // modeBuffer: the whole body; modeStream: unflushed bytes
	timer       stopper
	err         error
}

func newResponseWriter(ctx context.Context, out sender, id string, after afterFunc) *responseWriter {
	if after == nil {
		after = realAfterFunc
	}
	return &responseWriter{ctx: ctx, out: out, id: id, after: after, mergeDelay: 10 * time.Millisecond, header: http.Header{}}
}

func (w *responseWriter) Header() http.Header { return w.header }

func isEventStream(h http.Header) bool {
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	return err == nil && mt == "text/event-stream"
}

func (w *responseWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeHeaderLocked(code)
}

func (w *responseWriter) writeHeaderLocked(code int) {
	if w.wroteHeader {
		return
	}
	if code >= 100 && code < 200 {
		return // informational responses aren't relayed
	}
	w.wroteHeader = true
	w.status = code
	if isEventStream(w.header) {
		w.startStreamLocked()
	}
}

func (w *responseWriter) live() error {
	if w.err != nil {
		return w.err
	}
	if w.ctx.Err() != nil {
		w.err = errCancelled
		return w.err
	}
	return nil
}

// startStreamLocked sends the response head with stream:true and moves any buffered body to pending.
func (w *responseWriter) startStreamLocked() {
	if w.mode != modeBuffer || w.live() != nil {
		return
	}
	w.mode = modeStream
	w.header.Del("Content-Length")
	if err := w.out.send(w.ctx, responseFrame{V: 1, Type: "response", ID: w.id, Status: w.status,
		Headers: filterResponseHeaders(w.header), Stream: true}); err != nil {
		w.err = err
	}
}

func (w *responseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.live(); err != nil {
		return 0, err
	}
	w.writeHeaderLocked(http.StatusOK)
	switch w.mode {
	case modeDone:
		return 0, errors.New("write after the response finished")
	case modeBuffer:
		w.buf.Write(p)
		if w.buf.Len() > WholeMaxBytes {
			w.startStreamLocked()
			w.flushLocked()
		}
	case modeStream:
		w.buf.Write(p)
		if w.buf.Len() >= MergeBytes {
			w.flushLocked()
		} else if w.timer == nil {
			w.timer = w.after(w.mergeDelay, w.timerFlush)
		}
	}
	if w.err != nil {
		return 0, w.err
	}
	return len(p), nil
}

func (w *responseWriter) timerFlush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timer = nil
	if w.mode == modeStream {
		w.flushLocked()
	}
}

// flushLocked sends pending bytes as chunks of at most 1 MiB.
func (w *responseWriter) flushLocked() {
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	for w.buf.Len() > 0 && w.live() == nil {
		n := min(w.buf.Len(), MaxChunk)
		data := base64.StdEncoding.EncodeToString(w.buf.Next(n))
		if err := w.out.send(w.ctx, chunkFrame{V: 1, Type: "chunk", ID: w.id, Data: data}); err != nil {
			w.err = err
		}
	}
	if w.live() != nil {
		w.buf.Reset()
	}
}

// Flush marks the response as streaming when its length is unknown; merged chunks go out on the 10 ms timer.
func (w *responseWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.live() != nil {
		return
	}
	w.writeHeaderLocked(http.StatusOK)
	if w.mode == modeBuffer && w.header.Get("Content-Length") == "" {
		w.startStreamLocked()
		if w.buf.Len() > 0 && w.timer == nil {
			w.timer = w.after(w.mergeDelay, w.timerFlush)
		}
	}
}

// finish sends the whole response, or the final chunk and end. failure, if non-empty, ends a stream with an
// error (or turns an unstarted response into a 502).
func (w *responseWriter) finish(failure string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.live() != nil {
		w.mode = modeDone
		return
	}
	switch w.mode {
	case modeBuffer:
		status := w.status
		if !w.wroteHeader {
			status = http.StatusOK
		}
		body := w.buf.Bytes()
		if failure != "" && !w.wroteHeader {
			status, body = http.StatusBadGateway, []byte("toprelay agent: "+failure+"\n")
			w.header = http.Header{"Content-Type": {"text/plain; charset=utf-8"}}
		}
		w.header.Set("Content-Length", strconv.Itoa(len(body)))
		f := responseFrame{V: 1, Type: "response", ID: w.id, Status: status, Headers: filterResponseHeaders(w.header)}
		if len(body) > 0 {
			f.Body = base64.StdEncoding.EncodeToString(body)
		}
		if err := w.out.send(w.ctx, f); err != nil {
			w.err = err
		}
	case modeStream:
		w.flushLocked()
		if w.live() == nil {
			if err := w.out.send(w.ctx, endFrame{V: 1, Type: "end", ID: w.id, Error: failure}); err != nil {
				w.err = err
			}
		}
	}
	w.mode = modeDone
}
