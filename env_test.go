package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeToEnvFile works on ".env" in the working directory, so each test runs in
// its own.
func inTempDir(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("couldn't read working directory: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("couldn't change to temp directory: %v", err)
	}
	t.Cleanup(func() { os.Chdir(original) })
}

func TestWriteToEnvFileCreatesFile(t *testing.T) {
	inTempDir(t)

	if err := writeToEnvFile("token-1"); err != nil {
		t.Fatalf("writeToEnvFile() returned error: %v", err)
	}

	content, err := os.ReadFile(".env")
	if err != nil {
		t.Fatalf("couldn't read the .env that was written: %v", err)
	}
	if got := strings.TrimSpace(string(content)); got != "SPOTIFY_REFRESH_TOKEN=token-1" {
		t.Errorf(".env = %q, want the refresh token line", got)
	}
}

func TestWriteToEnvFileKeepsOtherValues(t *testing.T) {
	inTempDir(t)

	existing := "SPOTIFY_CLIENT_ID=abc\nSPOTIFY_CLIENT_SECRET=shh\n"
	if err := os.WriteFile(".env", []byte(existing), 0600); err != nil {
		t.Fatalf("couldn't seed .env: %v", err)
	}

	if err := writeToEnvFile("token-1"); err != nil {
		t.Fatalf("writeToEnvFile() returned error: %v", err)
	}

	content, err := os.ReadFile(".env")
	if err != nil {
		t.Fatalf("couldn't read .env: %v", err)
	}

	for _, want := range []string{"SPOTIFY_CLIENT_ID=abc", "SPOTIFY_CLIENT_SECRET=shh", "SPOTIFY_REFRESH_TOKEN=token-1"} {
		if !strings.Contains(string(content), want) {
			t.Errorf(".env is missing %q, got:\n%s", want, content)
		}
	}
}

func TestWriteToEnvFileReplacesExistingToken(t *testing.T) {
	inTempDir(t)

	existing := "SPOTIFY_CLIENT_ID=abc\nSPOTIFY_REFRESH_TOKEN=old\n"
	if err := os.WriteFile(".env", []byte(existing), 0600); err != nil {
		t.Fatalf("couldn't seed .env: %v", err)
	}

	if err := writeToEnvFile("new"); err != nil {
		t.Fatalf("writeToEnvFile() returned error: %v", err)
	}

	content, err := os.ReadFile(".env")
	if err != nil {
		t.Fatalf("couldn't read .env: %v", err)
	}
	if strings.Contains(string(content), "SPOTIFY_REFRESH_TOKEN=old") {
		t.Errorf("the old token is still there:\n%s", content)
	}
	if strings.Count(string(content), "SPOTIFY_REFRESH_TOKEN=") != 1 {
		t.Errorf("want exactly one refresh token line, got:\n%s", content)
	}
}

// An unreadable .env must not be replaced with one holding only the refresh
// token: that would throw away the client credentials it was holding.
func TestWriteToEnvFileRefusesToClobberUnreadableFile(t *testing.T) {
	inTempDir(t)

	existing := "SPOTIFY_CLIENT_ID=abc\nSPOTIFY_CLIENT_SECRET=shh\n"
	if err := os.WriteFile(".env", []byte(existing), 0000); err != nil {
		t.Fatalf("couldn't seed .env: %v", err)
	}
	if _, err := os.ReadFile(".env"); err == nil {
		t.Skip("running as a user that can read a 0000 file, so this can't be tested here")
	}

	if err := writeToEnvFile("token-1"); err == nil {
		t.Fatal("writeToEnvFile() succeeded on an unreadable .env, want an error")
	}

	if err := os.Chmod(".env", 0600); err != nil {
		t.Fatalf("couldn't restore permissions: %v", err)
	}
	content, err := os.ReadFile(".env")
	if err != nil {
		t.Fatalf("couldn't read .env: %v", err)
	}
	if string(content) != existing {
		t.Errorf("the credentials were overwritten:\n%s", content)
	}
}

func TestWriteToEnvFileIsNotWorldReadable(t *testing.T) {
	inTempDir(t)

	if err := writeToEnvFile("token-1"); err != nil {
		t.Fatalf("writeToEnvFile() returned error: %v", err)
	}

	info, err := os.Stat(filepath.Join(".", ".env"))
	if err != nil {
		t.Fatalf("couldn't stat .env: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf(".env mode = %o, want 600: it holds a refresh token", perm)
	}
}

func TestAllowedOriginsIncludesTheSite(t *testing.T) {
	t.Setenv("ALLOWED_ORIGINS", "")

	origins := allowedOrigins()
	if len(origins) != 2 {
		t.Fatalf("allowedOrigins() = %v, want just the two site origins", origins)
	}
}

func TestAllowedOriginsAppendsExtras(t *testing.T) {
	t.Setenv("ALLOWED_ORIGINS", "http://localhost:8081, http://127.0.0.1:3000")

	origins := allowedOrigins()
	for _, want := range []string{"http://localhost:8081", "http://127.0.0.1:3000"} {
		if !slicesContains(origins, want) {
			t.Errorf("allowedOrigins() = %v, missing %q", origins, want)
		}
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// A .env copied from the template is 0644, and os.WriteFile's permissions only
// apply to a file it creates — so writing the token into one left it readable
// by anything else on the machine.
func TestWriteToEnvFileTightensAnExistingFile(t *testing.T) {
	inTempDir(t)

	if err := os.WriteFile(".env", []byte("SPOTIFY_CLIENT_ID=abc\n"), 0644); err != nil {
		t.Fatalf("couldn't seed .env: %v", err)
	}

	if err := writeToEnvFile("token-1"); err != nil {
		t.Fatalf("writeToEnvFile() returned error: %v", err)
	}

	info, err := os.Stat(".env")
	if err != nil {
		t.Fatalf("couldn't stat .env: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf(".env mode = %o, want 600: an existing world-readable file kept its permissions", perm)
	}
}

// If the replacement fails, the credentials that were there have to still be
// there — and the half-written copy holding a refresh token must not be left
// lying around.
func TestWriteToEnvFileKeepsCredentialsWhenReplacementFails(t *testing.T) {
	inTempDir(t)

	existing := "SPOTIFY_CLIENT_ID=abc\nSPOTIFY_REFRESH_TOKEN=old\n"
	if err := os.WriteFile(".env", []byte(existing), 0600); err != nil {
		t.Fatalf("couldn't seed .env: %v", err)
	}

	original := renameFile
	renameFile = func(string, string) error { return errors.New("no space left on device") }
	t.Cleanup(func() { renameFile = original })

	if err := writeToEnvFile("new"); err == nil {
		t.Fatal("writeToEnvFile() succeeded though the replacement failed")
	}

	content, err := os.ReadFile(".env")
	if err != nil {
		t.Fatalf("couldn't read .env: %v", err)
	}
	if string(content) != existing {
		t.Errorf("the original credentials were lost:\n%s", content)
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("couldn't list the directory: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != ".env" {
			t.Errorf("left %q behind, which holds the refresh token", entry.Name())
		}
	}
}
