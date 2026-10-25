package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	err := os.WriteFile(path, []byte(`
control: /tmp/test.sock
services:
  web:
    listen: ":8080"
    health: "http://127.0.0.1:{port}/health"
    drain: 10s
    command:
      - /bin/echo
      - "{port}"
`), 0644)
	if err != nil {
		t.Fatal(err)
	}
	f, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Control != "/tmp/test.sock" {
		t.Fatalf("control %q", f.Control)
	}
	spec := f.Services["web"]
	spec.ApplyDefaults()
	if time.Duration(spec.Drain) != 10*time.Second {
		t.Fatalf("drain %v", spec.Drain)
	}
	if time.Duration(spec.HealthTimeout) != 30*time.Second {
		t.Fatalf("default health timeout")
	}
	if !*spec.AutoRestart {
		t.Fatal("auto_restart default")
	}
}

func TestLoadFileValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	os.WriteFile(path, []byte(`services: {}`), 0644)
	if _, err := LoadFile(path); err == nil {
		t.Fatal("expected empty services error")
	}
}
