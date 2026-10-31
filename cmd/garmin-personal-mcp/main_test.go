package main

import (
	"context"
	"errors"
	"slices"
	"syscall"
	"testing"
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
	}
	for _, tc := range cases {
		if got := run(tc.args); got != tc.want {
			t.Errorf("run(%q) = %d, want %d", tc.args, got, tc.want)
		}
	}
}

func TestReorderPutsFlagsFirst(t *testing.T) {
	got := reorder([]string{"auth", "--check"})
	if want := []string{"--check", "auth"}; !slices.Equal(got, want) {
		t.Errorf("reorder = %q, want %q", got, want)
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
