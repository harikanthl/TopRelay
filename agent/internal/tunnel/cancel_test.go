package tunnel_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/harikanthl/toprelay/agent/internal/tunnel"
)

// A cancel for one request that lands while that request's frame is being written (the relay is slow to read)
// must not close the shared socket: the other in-flight request still gets its answer.
func TestCancelDuringWriteKeepsSocket(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		chunk := []byte(strings.Repeat("A", 1<<20))
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	})
	mux.HandleFunc("/small", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second) // answers after the cancel for /big
		w.Write([]byte("ok"))
	})

	result := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(-1)
		ctx := context.Background()
		if _, _, err := c.Read(ctx); err != nil { // hello
			result <- "hello: " + err.Error()
			return
		}
		send := func(v string) { c.Write(ctx, websocket.MessageText, []byte(v)) }
		send(`{"v":1,"type":"welcome","caps":[],"maxBody":20971520}`)
		send(`{"v":1,"type":"request","id":"big","method":"GET","path":"/big","headers":{}}`)
		send(`{"v":1,"type":"request","id":"small","method":"GET","path":"/small","headers":{}}`)
		time.Sleep(400 * time.Millisecond) // not reading: the agent's chunk writes block
		send(`{"v":1,"type":"cancel","id":"big"}`)
		time.Sleep(200 * time.Millisecond)
		for {
			_, msg, err := c.Read(ctx)
			if err != nil {
				result <- "socket closed before the other request answered: " + err.Error()
				return
			}
			var f struct{ Type, ID string }
			json.Unmarshal(msg, &f)
			if f.ID == "small" && f.Type == "response" {
				result <- "ok"
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tunnel.New(tunnel.Options{BaseURL: srv.URL, Service: "notes", AgentKey: "k", Handler: mux}).Run(ctx)
	select {
	case got := <-result:
		if got != "ok" {
			t.Fatal(got)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no answer for the other request")
	}
}
