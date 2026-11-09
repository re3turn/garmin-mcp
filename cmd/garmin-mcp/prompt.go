package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

var (
	errInterrupted = errors.New("interrupted")
	errEscaped     = errors.New("cancelled with Esc")
)

// readSecret reads a line at the terminal in raw mode. term.ReadPassword shows
// nothing, so a paste looks lost, and leaves Ctrl-C to the signal handler,
// which can't stop the read before Enter.
func readSecret(ctx context.Context, fd int) (string, error) {
	return readTerminalLine(ctx, fd, true)
}

// readVisible is readSecret for input that isn't secret, such as an email or a
// one-time code, so Esc and Ctrl-C cancel it the same way.
func readVisible(ctx context.Context, fd int) (string, error) {
	return readTerminalLine(ctx, fd, false)
}

func readTerminalLine(ctx context.Context, fd int, masked bool) (string, error) {
	state, err := term.MakeRaw(fd)
	if err != nil {
		return "", err
	}
	defer term.Restore(fd, state)
	type result struct {
		line string
		err  error
	}
	read := make(chan result, 1)
	go func() {
		line, err := readTyped(os.Stdin, os.Stderr, masked)
		read <- result{line, err}
	}()
	select {
	case r := <-read:
		fmt.Fprint(os.Stderr, "\r\n")
		return r.line, r.err
	case <-ctx.Done():
		// Such as SIGTERM; the process exits, ending the blocked read.
		fmt.Fprint(os.Stderr, "\r\n")
		return "", ctx.Err()
	}
}

// readMasked echoes * for each character so the user sees the key arrive.
func readMasked(in io.Reader, out io.Writer) (string, error) {
	return readTyped(in, out, true)
}

// readTyped echoes * for each character when masked, and the character itself
// otherwise.
func readTyped(in io.Reader, out io.Writer, masked bool) (string, error) {
	var line []byte
	buf := make([]byte, 256)
	for {
		n, err := in.Read(buf)
		chunk := buf[:n]
		// An arrow or function key arrives as Esc and more in one read; only a
		// lone Esc cancels.
		if len(chunk) > 1 && chunk[0] == 0x1b {
			chunk = nil
		}
		for _, b := range chunk {
			switch {
			case b == '\r' || b == '\n':
				return string(line), nil
			case b == 0x03: // Ctrl-C
				return "", errInterrupted
			case b == 0x1b:
				return "", errEscaped
			case b == 0x04 && len(line) == 0: // Ctrl-D
				return "", nil
			case b == 0x7f || b == '\b':
				if len(line) > 0 {
					line = line[:len(line)-1]
					fmt.Fprint(out, "\b \b")
				}
			case b == 0x15: // Ctrl-U
				fmt.Fprint(out, strings.Repeat("\b \b", len(line)))
				line = line[:0]
			case b >= 0x20:
				line = append(line, b)
				if masked {
					fmt.Fprint(out, "*")
				} else {
					out.Write([]byte{b})
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return string(line), nil
		}
		if err != nil {
			return "", err
		}
	}
}
