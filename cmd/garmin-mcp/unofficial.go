package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/re3turn/garmin-mcp/internal/garmin"
)

const unofficialWarning = `This signs in to the unofficial Garmin Connect API, the one Garmin's own apps use.
Garmin can change or block it at any time, and using it may conflict with Garmin's terms.
Your password is sent to Garmin only and is never saved; the tokens Garmin returns are
saved encrypted and give full access to your account.

`

// signIn reads the email, password, and any MFA code at the terminal. A
// password read from a pipe tends to end up in shell history or a log, so a
// pipe is refused.
func signIn(ctx context.Context) int {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		fmt.Fprintln(os.Stderr, "Signing in needs a terminal to type the password in; run `garmin-mcp auth --unofficial` in one.")
		return 1
	}
	connect, err := garmin.NewConnectClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fmt.Fprint(os.Stderr, unofficialWarning)
	fmt.Fprint(os.Stderr, "Garmin email (Esc or Ctrl-C cancels): ")
	email, err := readVisible(ctx, fd)
	if code, done := promptEnded(ctx, err); done {
		return code
	}
	email = strings.TrimSpace(email)
	fmt.Fprint(os.Stderr, "Garmin password: ")
	password, err := readSecret(ctx, fd)
	if code, done := promptEnded(ctx, err); done {
		return code
	}
	if email == "" || password == "" {
		fmt.Fprintln(os.Stderr, "Email and password are both needed; nothing saved.")
		return 1
	}
	path, err := connect.Login(ctx, email, password, garmin.LoginPrompts{
		MFACode: func(ctx context.Context, method string, retry bool) (string, error) {
			if retry {
				fmt.Fprint(os.Stderr, "Try the code again: ")
			} else {
				fmt.Fprintf(os.Stderr, "Enter the code Garmin sent (%s): ", method)
			}
			code, err := readVisible(ctx, fd)
			if err != nil {
				return "", err
			}
			return code, nil
		},
		Progress: func(message string) { fmt.Fprintln(os.Stderr, message) },
	})
	if err != nil {
		// A cancelled MFA prompt ends the sign-in the same way the others do.
		if ctx.Err() != nil || errors.Is(err, errInterrupted) || errors.Is(err, errEscaped) {
			code, _ := promptEnded(ctx, err)
			return code
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("Signed in to Garmin Connect. Tokens saved encrypted to %s.\n", path)
	fmt.Println("Start the server with --unofficial to use them.")
	return 0
}

// promptEnded reports how to exit when a prompt was cancelled or failed.
func promptEnded(ctx context.Context, err error) (int, bool) {
	switch {
	case ctx.Err() != nil:
		fmt.Fprintln(os.Stderr, "Cancelled; nothing saved.")
		return cancelledExitCode(ctx), true
	case errors.Is(err, errInterrupted):
		fmt.Fprintln(os.Stderr, "Cancelled; nothing saved.")
		return 130, true
	case errors.Is(err, errEscaped):
		fmt.Fprintln(os.Stderr, "Cancelled; nothing saved.")
		return 1, true
	case err != nil:
		fmt.Fprintf(os.Stderr, "Couldn't read the input: %v\n", err)
		return 1, true
	}
	return 0, false
}

// warnIfNotSignedIn says at startup what the unofficial tools' first call
// would otherwise be the first to report. The server still starts: the
// Personal API tools work without the sign-in, and one can sign in while it
// runs, since the token file is read whenever no usable token is in memory.
func warnIfNotSignedIn() {
	if err := garmin.CheckConnectSignIn(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning (--unofficial): %v The other tools work as usual.\n", err)
	}
}

func checkUnofficial(ctx context.Context) int {
	connect, err := garmin.NewConnectClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if _, err := connect.Profile(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "The saved Garmin Connect sign-in doesn't work: %v\n", err)
		return 1
	}
	fmt.Println("Signed in to Garmin Connect (unofficial API).")
	return 0
}
