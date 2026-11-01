package service

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/harikanthl/toprelay/agent/internal/config"
)

// Concurrent policy changes and tool discovery must leave the remembered hello equal to the real one, or a later
// change back to it would be wrongly suppressed and the relay would keep a stale allow-list.
func TestNotifyRemembersCurrentHello(t *testing.T) {
	svc := config.Service{Name: "notes", Kind: "mcp", Gen: 1, Source: config.Source{Type: config.SourceMCPURL, URL: "http://127.0.0.1:1/mcp"},
		Tools: config.ToolPolicy{Default: config.DefaultAllow, Deny: []string{"x"}}}
	for round := 0; round < 300; round++ {
		rt, err := Build(svc, "", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for range rt.HelloChanged() {
			}
		}()
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				rt.addKnown([]string{fmt.Sprintf("t%d", i)})
			}()
			go func() {
				defer wg.Done()
				deny := []string{"x"}
				if i%2 == 0 {
					deny = append(deny, fmt.Sprintf("t%d", i-1))
				}
				rt.SetPolicy(config.ToolPolicy{Default: config.DefaultAllow, Deny: deny})
			}()
		}
		wg.Wait()
		now := rt.Hello()
		rt.mu.Lock()
		last := rt.lastHello
		rt.mu.Unlock()
		if last == nil || last.AllowAll != now.AllowAll || !slices.Equal(last.Tools, now.Tools) {
			t.Fatalf("round %d: remembered hello %+v, current %+v", round, last, now)
		}
		close(rt.helloChanged)
	}
}
