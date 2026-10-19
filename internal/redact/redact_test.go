package redact

import (
	"strings"
	"testing"
)

func TestEgressRedaction(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "plain text",
			input:    "All unit tests passed successfully.",
			expected: "All unit tests passed successfully.",
		},
		{
			name:     "github token",
			input:    "Pushed commit with token ghp_12345678901234567890abcdef to origin",
			expected: "Pushed commit with token " + GHMarker + " to origin",
		},
		{
			name:     "aws key",
			input:    "Using key AKIAIOSFODNN7EXAMPLE for s3 upload",
			expected: "Using key " + AWSMarker + " for s3 upload",
		},
		{
			name:     "openai key",
			input:    "Configured sk-live-12345678abcdefgh in env",
			expected: "Configured " + APIMarker + " in env",
		},
		{
			name:     "jwt token",
			input:    "Auth response eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.e30.t-IDcSemACt8x4iTMCda8Yhe3iZaWbvV5XKSTbuAn0M",
			expected: "Auth response " + JWTMarker,
		},
		{
			name:     "labeled password",
			input:    "database connection password=secretPassword123 host=localhost",
			expected: "database connection password=" + Marker + " host=localhost",
		},
		{
			name:     "json secret",
			input:    `{"api_key": "super-secret-key-123", "status": "ok"}`,
			expected: `{"api_key": "[REDACTED]", "status": "ok"}`,
		},
		{
			name:     "email",
			input:    "Author: dev.user@example.com committed changes",
			expected: "Author: " + EmailMarker + " committed changes",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Egress(tc.input)
			if got != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, got)
			}
			// Test idempotency
			second := Egress(got)
			if second != got {
				t.Errorf("idempotency check failed: expected %q, got %q", got, second)
			}
		})
	}
}

func TestEgressDoesNotRedactNormalWords(t *testing.T) {
	input := "The skill is located at internal/prompt/executor.go"
	got := Egress(input)
	if !strings.Contains(got, "internal/prompt/executor.go") {
		t.Errorf("unexpected redaction of non-secret path: %s", got)
	}
}
