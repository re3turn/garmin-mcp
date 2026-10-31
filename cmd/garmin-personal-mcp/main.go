// Command garmin-personal-mcp serves the Garmin Connect Personal API to MCP
// clients over stdio.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/term"

	"github.com/re3turn/garmin-personal-mcp/internal/garmin"
	"github.com/re3turn/garmin-personal-mcp/internal/server"
)

// version is set by -ldflags "-X main.version=..." in release builds.
var version = ""

const usage = `Usage:
  garmin-personal-mcp [serve]       serve over stdio (default)
  garmin-personal-mcp auth          save a Garmin Personal API key
  garmin-personal-mcp auth --check  report whether the saved key works
  garmin-personal-mcp --version
`

func main() {
	// stdout belongs to the MCP protocol, so logs go to stderr.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("garmin-personal-mcp", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	showVersion := flags.Bool("version", false, "print the version")
	check := flags.Bool("check", false, "with auth: report whether the saved key works, without prompting")
	if err := flags.Parse(reorder(args)); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println(buildVersion())
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	command := flags.Arg(0)
	if *check && command != "auth" {
		fmt.Fprintln(os.Stderr, "--check only applies to the auth command")
		return 2
	}
	switch command {
	case "", "serve":
		return serve(ctx)
	case "auth":
		if *check {
			return checkKey(ctx)
		}
		return saveKey(ctx)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", command, usage)
		return 2
	}
}

// reorder moves flags ahead of the command so `auth --check` parses; the flag
// package stops at the first non-flag argument.
func reorder(args []string) []string {
	var flags, rest []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
		} else {
			rest = append(rest, arg)
		}
	}
	return append(flags, rest...)
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

func serve(ctx context.Context) int {
	err := server.New(garmin.NewClient(), buildVersion()).Run(ctx, &mcp.StdioTransport{})
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("server stopped", "error", err)
		return 1
	}
	return 0
}

func saveKey(ctx context.Context) int {
	key, err := readKey()
	// The signal handler absorbs Ctrl-C at the hidden prompt, which leaves the
	// terminal's echo intact; honor it once input returns.
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "Cancelled; nothing saved.")
		return 130
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Couldn't read the key: %v\n", err)
		return 1
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "No key entered; nothing saved. Run `garmin-personal-mcp auth` in a terminal, or pipe the key in on stdin.")
		return 1
	}
	if _, err := garmin.NewClient(garmin.WithAPIKey(key)).ProfileSettings(ctx); err != nil {
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

// readKey hides input at a terminal. Without one (a pipe, or a shell an agent
// runs) it reads one line from stdin, e.g. `pbpaste | garmin-personal-mcp auth`.
func readKey() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	fmt.Fprintf(os.Stderr, "Create a Garmin Connect Personal API key (it is shown only once):\n  %s\n\n", garmin.KeyPage)
	fmt.Fprint(os.Stderr, "Paste the key (input is hidden): ")
	key, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(key)), nil
}

func checkKey(ctx context.Context) int {
	if _, err := garmin.NewClient().ProfileSettings(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Not connected: %v\n", err)
		return 1
	}
	fmt.Println("Connected to Garmin Connect.")
	return 0
}
