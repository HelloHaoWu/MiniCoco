package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadEnvPreservesEnvironment(t *testing.T) {
	t.Setenv("LLM_API_KEY", "externally-set-test-key")
	t.Setenv("LLM_MODEL", "custom-model")
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("LLM_API_KEY=file-test-key\nLLM_MODEL=deepseek-flash\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LLM_API_KEY") != "externally-set-test-key" || os.Getenv("LLM_MODEL") != "custom-model" {
		t.Fatal("local config overrode the environment")
	}
}

func TestLoadEnvQuotesAndDoesNotLeakMalformedValues(t *testing.T) {
	previous, exists := os.LookupEnv("LLM_API_KEY")
	os.Unsetenv("LLM_API_KEY")
	t.Cleanup(func() {
		if exists {
			os.Setenv("LLM_API_KEY", previous)
		} else {
			os.Unsetenv("LLM_API_KEY")
		}
	})
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("# comment\nexport LLM_API_KEY='local-test-key'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LLM_API_KEY") != "local-test-key" {
		t.Fatal("quoted key was not loaded")
	}
	if err := os.WriteFile(path, []byte("LLM_API_KEY=\"private-invalid-test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	err := loadEnvFile(path)
	if err == nil || strings.Contains(err.Error(), "private-invalid-test-key") {
		t.Fatal("malformed config was accepted or its value leaked")
	}
}
