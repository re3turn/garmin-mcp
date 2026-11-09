// Command garmin-mcp serves Garmin Connect data to MCP clients over stdio, or
// over streamable HTTP with --http. It reads the Personal API, and with
// --unofficial also the unofficial Garmin Connect API for what that lacks.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/term"

	"github.com/re3turn/garmin-mcp/internal/garmin"
	"github.com/re3turn/garmin-mcp/internal/server"
)

// version is set by -ldflags "-X main.version=..." in release builds.
var version = ""

const usage = `Usage:
  garmin-mcp [serve]       serve over stdio (default)
  garmin-mcp [serve] --http [--host 127.0.0.1] [--port 8491] [--allowed-host NAME]...
                           serve streamable HTTP at /mcp (no authentication)
  garmin-mcp auth          save a Garmin Personal API key
  garmin-mcp auth --check  report whether the saved key works
  garmin-mcp --version

Unofficial Garmin Connect API (data the Personal API doesn't have; can break without notice):
  garmin-mcp auth --unofficial          sign in with your Garmin email and password
  garmin-mcp auth --unofficial --check  report whether the saved sign-in works
  garmin-mcp [serve] --unofficial       also serve the unofficial API's tools
  garmin-mcp [serve] --http --unofficial --allow-unofficial-over-http
                           the same over HTTP, which has no authentication
`

// hostList collects a flag that may be repeated.
type hostList []string

func (h *hostList) String() string { return strings.Join(*h, ",") }

func (h *hostList) Set(value string) error {
	*h = append(*h, value)
	return nil
}

func main() {
	// stdout belongs to the MCP protocol, so logs go to stderr.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("garmin-mcp", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	showVersion := flags.Bool("version", false, "print the version")
	check := flags.Bool("check", false, "with auth: report whether the saved key works, without prompting")
	useHTTP := flags.Bool("http", false, "serve streamable HTTP instead of stdio")
	host := flags.String("host", "127.0.0.1", "with --http: address to listen on")
	port := flags.Int("port", 8491, "with --http: port to listen on")
	var allowedHosts hostList
	flags.Var(&allowedHosts, "allowed-host", "with --http: another Host header to accept, such as a reverse proxy's hostname (repeatable)")
	unofficial := flags.Bool("unofficial", false, "with serve: add the unofficial Garmin Connect API's tools; with auth: sign in to it")
	unofficialOverHTTP := flags.Bool("allow-unofficial-over-http", false, "with --http --unofficial: serve the unofficial tools over HTTP anyway")
	positional, err := parseInterspersed(flags, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Println(buildVersion())
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var command string
	if len(positional) > 0 {
		command = positional[0]
	}
	if len(positional) > 1 {
		// `auth check` without the dashes would otherwise start the prompt.
		fmt.Fprintf(os.Stderr, "unexpected arguments %q\n\n%s", positional[1:], usage)
		return 2
	}
	if *check && command != "auth" {
		fmt.Fprintln(os.Stderr, "--check only applies to the auth command")
		return 2
	}
	if *useHTTP && command == "auth" {
		fmt.Fprintf(os.Stderr, "--http only applies to serve\n\n%s", usage)
		return 2
	}
	misplaced := ""
	flags.Visit(func(f *flag.Flag) {
		if (f.Name == "host" || f.Name == "port" || f.Name == "allowed-host") && !*useHTTP {
			misplaced = f.Name
		}
	})
	if misplaced != "" {
		fmt.Fprintf(os.Stderr, "--%s needs --http\n\n%s", misplaced, usage)
		return 2
	}
	for _, name := range allowedHosts {
		if err := server.ValidateAllowedHost(name); err != nil {
			fmt.Fprintf(os.Stderr, "--allowed-host: %v\n", err)
			return 2
		}
	}
	if *unofficialOverHTTP && (!*useHTTP || !*unofficial) {
		fmt.Fprintf(os.Stderr, "--allow-unofficial-over-http needs --http and --unofficial\n\n%s", usage)
		return 2
	}
	// The Connect tokens give full access to the account, unlike a read-only
	// Personal API key, so serving them where anyone who reaches the port can
	// use them takes a second, explicit flag.
	if *useHTTP && *unofficial && !*unofficialOverHTTP {
		fmt.Fprintln(os.Stderr, "--unofficial over --http serves tools backed by your Garmin account sign-in to anyone who "+
			"can reach the port, since HTTP mode has no authentication. Add --allow-unofficial-over-http if that is what you want.")
		return 2
	}
	switch command {
	case "", "serve":
		var opts []server.Option
		if *unofficial {
			connect, err := garmin.NewConnectClient()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			opts = append(opts, server.WithUnofficial(connect))
			warnIfNotSignedIn()
		}
		if *useHTTP {
			return serveHTTP(ctx, net.JoinHostPort(*host, strconv.Itoa(*port)), allowedHosts, opts...)
		}
		return serve(ctx, opts...)
	case "auth":
		switch {
		case *unofficial && *check:
			return checkUnofficial(ctx)
		case *unofficial:
			return signIn(ctx)
		case *check:
			return checkKey(ctx)
		}
		return saveKey(ctx)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", command, usage)
		return 2
	}
}

// parseInterspersed lets flags follow the command, as in `auth --check` or
// `serve --http --port 9000`: the flag package stops at the first non-flag
// argument, so parsing resumes after each one. Everything after a "--"
// terminator stays positional.
func parseInterspersed(flags *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}
		rest := flags.Args()
		if consumed := args[:len(args)-len(rest)]; len(consumed) > 0 && consumed[len(consumed)-1] == "--" {
			return append(positional, rest...), nil
		}
		args = rest
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func buildVersion() string {
	if version != "" {
		return version
	}
	// `go install module@version` records the version; a local build says "(devel)".
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func serve(ctx context.Context, opts ...server.Option) int {
	err := server.New(garmin.NewClient(), buildVersion(), opts...).Run(ctx, &mcp.StdioTransport{})
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("server stopped", "error", err)
		return 1
	}
	return 0
}

// shutdownGrace is how long in-flight tool calls get after Ctrl-C or SIGTERM.
const shutdownGrace = 5 * time.Second

func serveHTTP(ctx context.Context, addr string, allowedHosts []string, opts ...server.Option) int {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Couldn't listen on %s: %v\n", addr, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "Serving MCP over streamable HTTP at http://%s%s (no authentication; keep it off the public internet)\n",
		listener.Addr(), server.MCPPath)
	if err := runHTTP(ctx, listener, server.NewRemote(garmin.NewClient(), buildVersion(), opts...), allowedHosts, shutdownGrace); err != nil {
		slog.Error("server stopped", "error", err)
		return 1
	}
	return 0
}

// runHTTP serves until ctx is done. Closing the MCP sessions ends the event
// streams clients hold open, which Shutdown would otherwise wait on; grace
// bounds a tool call that never returns, and running it out is a normal stop.
func runHTTP(ctx context.Context, listener net.Listener, mcpServer *mcp.Server, allowedHosts []string, grace time.Duration) error {
	httpServer := &http.Server{Handler: server.HTTPHandler(mcpServer, allowedHosts), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	// A second Ctrl-C exits at once.
	signal.Reset(os.Interrupt, syscall.SIGTERM)
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	shutdown := make(chan error, 1)
	go func() { shutdown <- httpServer.Shutdown(shutdownCtx) }()
	// Close waits for in-flight requests, so it mustn't hold up the return.
	go func() {
		for session := range mcpServer.Sessions() {
			session.Close()
		}
	}()
	if err := <-shutdown; err != nil {
		httpServer.Close()
		if !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	}
	return nil
}

func saveKey(ctx context.Context) int {
	key, err := readKey(ctx)
	switch {
	case ctx.Err() != nil:
		fmt.Fprintln(os.Stderr, "Cancelled; nothing saved.")
		return cancelledExitCode(ctx)
	case errors.Is(err, errInterrupted):
		fmt.Fprintln(os.Stderr, "Cancelled; nothing saved.")
		return 130
	case errors.Is(err, errEscaped):
		fmt.Fprintln(os.Stderr, "Cancelled; nothing saved.")
		return 1
	case err != nil:
		fmt.Fprintf(os.Stderr, "Couldn't read the key: %v\n", err)
		return 1
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "No key entered; nothing saved. Run `garmin-mcp auth` in a terminal, or pipe the key in on stdin.")
		return 1
	}
	if _, err := garmin.NewClient(garmin.WithAPIKey(key)).ProfileSettings(ctx); err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "Cancelled; nothing saved.")
			return cancelledExitCode(ctx)
		}
		if errors.Is(err, garmin.ErrAPIKey) {
			fmt.Fprintf(os.Stderr, "Garmin rejected that key, so it wasn't saved. Check that you copied the whole key, "+
				"or create a new one at %s.\n", garmin.KeyPage)
		} else {
			fmt.Fprintf(os.Stderr, "Couldn't check the key with Garmin, so it wasn't saved: %v\n", err)
		}
		return 1
	}
	path, err := garmin.SaveAPIKey(key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Couldn't save the key: %v\n", err)
		return 1
	}
	fmt.Printf("Connected to Garmin Connect. Key saved to %s (owner-only permissions).\n", path)
	if _, ok := os.LookupEnv(garmin.KeyEnvVar); ok {
		fmt.Fprintf(os.Stderr, "Note: %s is set and takes precedence over the saved key; unset it to use the new one.\n", garmin.KeyEnvVar)
	}
	return 0
}

// cancelledExitCode follows the shell convention of 128 + signal number.
// signal.NotifyContext's cause type is unexported, so the signal is read from
// its message, which os/signal formats as "<signal> signal received".
func cancelledExitCode(ctx context.Context) int {
	if strings.HasPrefix(context.Cause(ctx).Error(), syscall.SIGTERM.String()+" ") {
		return 143
	}
	return 130
}

// readKey masks input at a terminal. Without one (a pipe, or a shell an agent
// runs) it reads one line from stdin, e.g. `pbpaste | garmin-mcp auth`.
func readKey(ctx context.Context) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	fmt.Fprintf(os.Stderr, "Create a Garmin Connect Personal API key (it is shown only once):\n  %s\n\n", garmin.KeyPage)
	fmt.Fprint(os.Stderr, "Paste the key and press Enter (Esc or Ctrl-C cancels): ")
	key, err := readSecret(ctx, fd)
	return strings.TrimSpace(key), err
}

func checkKey(ctx context.Context) int {
	if _, err := garmin.NewClient().ProfileSettings(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Not connected: %v\n", err)
		return 1
	}
	fmt.Println("Connected to Garmin Connect.")
	return 0
}
