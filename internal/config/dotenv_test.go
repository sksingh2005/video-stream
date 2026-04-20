package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(`
# comment
DOTENV_TEST_BUCKET=edtech
DOTENV_TEST_QUOTED="hello world"
DOTENV_TEST_INLINE=value # comment
`), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	t.Cleanup(func() {
		_ = os.Unsetenv("DOTENV_TEST_BUCKET")
		_ = os.Unsetenv("DOTENV_TEST_QUOTED")
		_ = os.Unsetenv("DOTENV_TEST_INLINE")
	})
	if err := loadDotEnv(path); err != nil {
		t.Fatalf("load .env: %v", err)
	}

	if got := os.Getenv("DOTENV_TEST_BUCKET"); got != "edtech" {
		t.Fatalf("expected bucket from .env, got %q", got)
	}
	if got := os.Getenv("DOTENV_TEST_QUOTED"); got != "hello world" {
		t.Fatalf("expected quoted value, got %q", got)
	}
	if got := os.Getenv("DOTENV_TEST_INLINE"); got != "value" {
		t.Fatalf("expected stripped inline comment, got %q", got)
	}
}
