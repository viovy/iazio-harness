// Package redact scrubs secrets and sensitive telemetry from prompt outputs, summaries, and error logs.
package redact

import (
	"regexp"
	"strings"
)

// Standard typed replacement markers.
const (
	Marker      = "[REDACTED]"
	AWSMarker   = "[REDACTED_AWS_KEY]"
	APIMarker   = "[REDACTED_API_KEY]"
	GHMarker    = "[REDACTED_GITHUB_TOKEN]"
	GLMarker    = "[REDACTED_GITLAB_TOKEN]"
	JWTMarker   = "[REDACTED_JWT]"
	EmailMarker = "[REDACTED_EMAIL]"
)

var labeledSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)([^\s"'\\]+)`),
	regexp.MustCompile(`(?i)(\bbearer\s+)([^\s"'\\]+)`),
	regexp.MustCompile(`(?i)(password\s*[=:]\s*)(\S+)`),
	regexp.MustCompile(`(?i)(token\s*[=:]\s*)(\S+)`),
	regexp.MustCompile(`(?i)(api[_-]?key\s*[=:]\s*)(\S+)`),
	regexp.MustCompile(`(?i)(secret\s*[=:]\s*)(\S+)`),
	// JSON object forms: "api_key":"…", "password": "…", "Authorization":"Bearer …"
	regexp.MustCompile(`(?i)("(?:api[_-]?key|password|token|secret)"\s*:\s*")([^"]+)(")`),
	regexp.MustCompile(`(?i)("authorization"\s*:\s*"\s*bearer\s+)([^"]+)(")`),
}

var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// Egress scrubs secrets and sensitive data from output strings.
// The operation is idempotent: Egress(Egress(s)) == Egress(s).
func Egress(s string) string {
	if s == "" {
		return s
	}
	out := s
	out = redactPrefixedToken(out, "AKIA", 16, AWSMarker, isAWSKeyChar)
	out = redactPrefixedToken(out, "ASIA", 16, AWSMarker, isAWSKeyChar)
	out = redactPrefixedToken(out, "sk-live-", 8, APIMarker, isTokenChar)
	out = redactPrefixedToken(out, "sk-test-", 8, APIMarker, isTokenChar)
	out = redactPrefixedToken(out, "sk-", 20, APIMarker, isTokenChar)
	out = redactPrefixedToken(out, "ghp_", 20, GHMarker, isTokenChar)
	out = redactPrefixedToken(out, "gho_", 20, GHMarker, isTokenChar)
	out = redactPrefixedToken(out, "ghu_", 20, GHMarker, isTokenChar)
	out = redactPrefixedToken(out, "glpat-", 20, GLMarker, isTokenChar)
	out = redactPrefixedToken(out, "eyJ", 20, JWTMarker, isTokenChar)
	for _, re := range labeledSecretPatterns {
		out = re.ReplaceAllStringFunc(out, func(m string) string {
			sub := re.FindStringSubmatch(m)
			switch len(sub) {
			case 3:
				if isAlreadyRedacted(sub[2]) {
					return m
				}
				return sub[1] + Marker
			case 4:
				if isAlreadyRedacted(sub[2]) {
					return m
				}
				return sub[1] + Marker + sub[3]
			default:
				return m
			}
		})
	}
	out = emailPattern.ReplaceAllStringFunc(out, func(m string) string {
		if m == EmailMarker || strings.HasPrefix(m, "[REDACTED") {
			return m
		}
		return EmailMarker
	})
	return out
}

func isAlreadyRedacted(s string) bool {
	return strings.HasPrefix(s, "[REDACTED")
}

func isAWSKeyChar(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= '2' && c <= '7')
}

func isTokenChar(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
		c == '_' || c == '-' || c == '.'
}

func redactPrefixedToken(s, prefix string, minTail int, replacement string, ok func(byte) bool) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if i+len(prefix) <= len(s) && s[i:i+len(prefix)] == prefix {
			end := i + len(prefix)
			for end < len(s) && ok(s[end]) {
				end++
			}
			if end-i >= len(prefix)+minTail {
				b.WriteString(replacement)
				i = end
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
