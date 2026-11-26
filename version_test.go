package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionDefaultDev(t *testing.T) {
	bin := buildPortswap(t, "")
	out := runBin(t, bin)
	if got := strings.TrimSpace(out); got != "portswap dev" {
		t.Fatalf("got %q", got)
	}
}

func TestVersionLdflags(t *testing.T) {
	bin := buildPortswap(t, "-X main.version=v0.2.0-test")
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		out := runBinArgs(t, bin, args...)
		if got := strings.TrimSpace(out); got != "portswap v0.2.0-test" {
			t.Fatalf("args %v: got %q", args, got)
		}
	}
}

func buildPortswap(t *testing.T, ldflag string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "portswap")
	args := []string{"build", "-o", bin, "."}
	if ldflag != "" {
		args = []string{"build", "-ldflags", ldflag, "-o", bin, "."}
	}
	if out, err := exec.Command("go", args...).CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func runBin(t *testing.T, bin string) string {
	return runBinArgs(t, bin, "--version")
}

func runBinArgs(t *testing.T, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatal(err)
		}
	}
	_ = os.Stderr // keep import
	return string(out)
}