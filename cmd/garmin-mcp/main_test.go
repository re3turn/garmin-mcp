package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/re3turn/garmin-mcp/internal/garmin"
	"github.com/re3turn/garmin-mcp/internal/server"
)

// Only arguments that exit before serving or prompting are exercised here.
func TestExitCodes(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"-h"}, 0},
		{[]string{"--version"}, 0},
		{[]string{"--check"}, 2},
		{[]string{"serve", "--check"}, 2},
		{[]string{"bogus"}, 2},
		{[]string{"auth", "check"}, 2},
		{[]string{"--no-such-flag"}, 2},
		{[]string{"--port", "9000"}, 2},
		{[]string{"serve", "--allowed-host", "proxy.example"}, 2},
		{[]string{"auth", "--http"}, 2},
		{[]string{"--http", "--allowed-host", "https://proxy.example"}, 2},
		{[]string{"--http", "--allowed-host", "--", "serve"}, 2},
	}
	for _, tc := range cases {
		if got := run(tc.args); got != tc.want {
			t.Errorf("run(%q) = %d, want %d", tc.args, got, tc.want)
		}
	}
}

func TestServeHTTPFailsWhenThePortIsTaken(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	if code := serveHTTP(context.Background(), taken.Addr().String(), nil); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

// A connected client keeps an event stream open. Closing the sessions ends it,
// so the server stops at once instead of running out the grace period, which
// is set long here so that waiting it out would fail the test.
func TestRunHTTPStopsAtOnceWithAClientConnected(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() {
		stopped <- runHTTP(ctx, listener, server.NewRemote(garmin.NewClient(), "test"), nil, 10*time.Second)
	}()

	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: "http://" + listener.Addr().String() + server.MCPPath}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	started := time.Now()
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("runHTTP = %v, want a normal stop", err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Errorf("stopping took %v, want it to stop at once", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runHTTP didn't stop")
	}
}

func TestParseInterspersed(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	check := flags.Bool("check", false, "")
	port := flags.Int("port", 0, "")
	var hosts hostList
	flags.Var(&hosts, "allowed-host", "")
	positional, err := parseInterspersed(flags, []string{
		"serve", "--port", "9000", "--allowed-host", "a.example", "--check", "--allowed-host=b.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(positional, []string{"serve"}) || *port != 9000 || !*check ||
		!slices.Equal(hosts, hostList{"a.example", "b.example"}) {
		t.Errorf("positional=%q port=%d check=%v hosts=%q", positional, *port, *check, hosts)
	}

	positional, err = parseInterspersed(flags, []string{"serve", "--", "x", "--port", "1"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"serve", "x", "--port", "1"}; !slices.Equal(positional, want) || *port != 9000 {
		t.Errorf("after --: positional=%q port=%d, want %q and port unchanged", positional, *port, want)
	}
}

func TestCancelledExitCode(t *testing.T) {
	cases := []struct {
		cause string
		want  int
	}{
		{syscall.SIGTERM.String() + " signal received", 143},
		{"interrupt signal received", 130},
	}
	for _, tc := range cases {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(errors.New(tc.cause))
		if got := cancelledExitCode(ctx); got != tc.want {
			t.Errorf("%s: exit code = %d, want %d", tc.cause, got, tc.want)
		}
	}
}
