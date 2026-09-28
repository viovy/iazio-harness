package main

import (
	"os"
	"strings"
	"testing"
)

func TestRunJobDefaults(t *testing.T) {
	// Missing job-id must error
	err := runJob([]string{})
	if err == nil || !strings.Contains(err.Error(), "job-id is required") {
		t.Fatalf("expected job-id required error, got %v", err)
	}

	// Environment variable fallback
	os.Setenv("IAZIO_HARNESS_API_URL", "http://custom-url")
	defer os.Unsetenv("IAZIO_HARNESS_API_URL")

	err = runJob([]string{})
	if err == nil || !strings.Contains(err.Error(), "job-id is required") {
		t.Fatalf("expected job-id required error with env set, got %v", err)
	}
}

func TestFormatVersion(t *testing.T) {
	v := formatVersion()
	if !strings.Contains(v, "iazio-harness") {
		t.Fatalf("expected iazio-harness version, got %s", v)
	}
}
