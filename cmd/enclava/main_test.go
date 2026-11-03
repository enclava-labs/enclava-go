package main

import (
	"context"
	"testing"
)

// TestTopLevelHelp: `enclava --help`, `-h` and `help` print usage and return
// success (OPS-I01). run() returns before config.Load, so no environment or
// database is needed to prove the path is config-free.
func TestTopLevelHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		if code := run(args); code != 0 {
			t.Errorf("run(%v) = %d, want 0", args, code)
		}
	}
}

// TestBootstrapHelp: `enclava bootstrap -h`/`--help` prints flag usage and
// succeeds instead of surfacing flag.ErrHelp as an error (OPS-I01).
func TestBootstrapHelp(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		if err := runBootstrap(context.Background(), []string{flag}); err != nil {
			t.Errorf("runBootstrap(%q) = %v, want nil", flag, err)
		}
	}
	if code := run([]string{"bootstrap", "-h"}); code != 0 {
		t.Errorf("run(bootstrap -h) = %d, want 0", code)
	}
}
