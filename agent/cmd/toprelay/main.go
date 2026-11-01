// Command toprelay is the TopRelay laptop agent: it deploys the relay Worker into your Cloudflare account and
// keeps local MCP servers and HTTP services reachable through it.
package main

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/term"

	"github.com/harikanthl/toprelay/agent/internal/cli"
)

func main() {
	app := &cli.App{
		Ctx:    context.Background(),
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Getenv: os.Getenv,
		IsTTY: func() bool {
			return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
		},
		ReadSecret: func(prompt string) (string, error) {
			fmt.Fprint(os.Stderr, prompt)
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			return string(b), err
		},
	}
	os.Exit(cli.Main(os.Args[1:], app))
}
