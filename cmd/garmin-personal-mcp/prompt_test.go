package main

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// chunks returns each string from its own Read, as a terminal in raw mode
// delivers one key press (or one paste) at a time.
type chunks []string

func (c *chunks) Read(p []byte) (int, error) {
	if len(*c) == 0 {
		return 0, io.EOF
	}
	n := copy(p, (*c)[0])
	*c = (*c)[1:]
	return n, nil
}

func TestReadMasked(t *testing.T) {
	cases := []struct {
		name    string
		input   chunks
		want    string
		wantErr error
		echo    string
	}{
		{"paste then Enter", chunks{"abc", "\r"}, "abc", nil, "***"},
		{"paste with newline", chunks{"abc\n"}, "abc", nil, "***"},
		{"backspace", chunks{"ab", "\x7f", "c", "\r"}, "ac", nil, "**\b \b*"},
		{"Ctrl-U", chunks{"ab", "\x15", "c", "\r"}, "c", nil, "**\b \b\b \b*"},
		{"Ctrl-C", chunks{"ab", "\x03"}, "", errInterrupted, "**"},
		{"lone Esc", chunks{"ab", "\x1b"}, "", errEscaped, "**"},
		{"arrow key ignored", chunks{"a", "\x1b[D", "b", "\r"}, "ab", nil, "**"},
		{"Ctrl-D on empty", chunks{"\x04"}, "", nil, ""},
		{"EOF", chunks{"ab"}, "ab", nil, "**"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var echo bytes.Buffer
			got, err := readMasked(&tc.input, &echo)
			if got != tc.want || !errors.Is(err, tc.wantErr) || (err == nil) != (tc.wantErr == nil) {
				t.Errorf("got %q, %v; want %q, %v", got, err, tc.want, tc.wantErr)
			}
			if echo.String() != tc.echo {
				t.Errorf("echo = %q, want %q", echo.String(), tc.echo)
			}
		})
	}
}
