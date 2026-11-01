package mcpproxy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// HandlerTransport is an http.RoundTripper that serves requests with an in-process handler, streaming the
// response body through a pipe. The stdio bridge uses it so the same guard and filter sit in front of go-sdk.
type HandlerTransport struct {
	Handler http.Handler
}

type pipeResponseWriter struct {
	header  http.Header
	pw      *io.PipeWriter
	once    sync.Once
	ready   chan struct{}
	status  int
	snap    http.Header
	wrote   bool
	writeMu sync.Mutex
}

func (w *pipeResponseWriter) Header() http.Header { return w.header }

func (w *pipeResponseWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		return
	}
	w.once.Do(func() {
		w.status = code
		w.snap = w.header.Clone()
		close(w.ready)
	})
}

func (w *pipeResponseWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	return w.pw.Write(p)
}

func (w *pipeResponseWriter) Flush() { w.WriteHeader(http.StatusOK) }

// RoundTrip runs the handler in a goroutine and returns once it has written its headers.
func (t HandlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	w := &pipeResponseWriter{header: http.Header{}, pw: pw, ready: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				pw.CloseWithError(fmt.Errorf("handler panic: %v", r))
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			pw.Close()
		}()
		t.Handler.ServeHTTP(w, req)
	}()
	select {
	case <-w.ready:
	case <-req.Context().Done():
		pr.CloseWithError(req.Context().Err())
		return nil, req.Context().Err()
	}
	return &http.Response{StatusCode: w.status, Status: http.StatusText(w.status), Header: w.snap, Body: &pipeBody{pr: pr, done: done},
		ContentLength: -1, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Request: req}, nil
}

type pipeBody struct {
	pr   *io.PipeReader
	done chan struct{}
}

func (b *pipeBody) Read(p []byte) (int, error) { return b.pr.Read(p) }

func (b *pipeBody) Close() error {
	b.pr.CloseWithError(errors.New("response body closed"))
	return nil
}
