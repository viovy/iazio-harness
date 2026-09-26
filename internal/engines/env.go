package engines

import "sort"

// Forced environment keys. Schedule values cannot override them.
const (
	EnvGitTerminalPrompt = "GIT_TERMINAL_PROMPT"
	EnvSSHBatchMode      = "SSH_BATCHMODE"
	EnvSSHAskPass        = "SSH_ASKPASS"
	EnvDocsHubPath       = "IAZIO_DOCS_HUB_PATH"
)

// ChildEnv merges the base environment, then schedule values, then forced keys.
// GIT_TERMINAL_PROMPT, SSH_BATCHMODE, SSH_ASKPASS, and IAZIO_DOCS_HUB_PATH are set last.
// Those keys are not inserted into the prompt; callers must not copy them into output chunks.
func ChildEnv(base []string, schedule map[string]string, docsHubPath string) []string {
	m := map[string]string{}
	for _, kv := range base {
		k, v, ok := splitEnv(kv)
		if !ok || k == "" {
			continue
		}
		m[k] = v
	}
	for k, v := range schedule {
		if k == "" {
			continue
		}
		m[k] = v
	}
	m[EnvGitTerminalPrompt] = "0"
	m[EnvSSHBatchMode] = "yes"
	m[EnvSSHAskPass] = ""
	m[EnvDocsHubPath] = docsHubPath
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

// EnvLookup returns the value of key in an environment slice.
func EnvLookup(env []string, key string) (string, bool) {
	for _, kv := range env {
		k, v, ok := splitEnv(kv)
		if ok && k == key {
			return v, true
		}
	}
	return "", false
}

func splitEnv(kv string) (string, string, bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return "", "", false
}
