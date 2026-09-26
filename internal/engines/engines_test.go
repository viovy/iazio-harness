package engines

import "testing"

func TestAgentArgv(t *testing.T) {
	a := Build("agent", "ordinary", "hello", "/repos/leaf-01", "/repos/docs-hub", "")
	if a.Cmd != "agent" || a.Cwd != "/repos/leaf-01" || a.Args[0] != "--print" || a.Args[len(a.Args)-1] != "hello" {
		t.Fatalf("%+v", a)
	}
	ref := Build("agy", "story_refinement", "hello", "/repos/leaf-01", "/repos/docs-hub", "log")
	if ref.Cwd != "/repos/docs-hub" || ref.Args[0] != "--print" || ref.Args[1] != "hello" {
		t.Fatalf("%+v", ref)
	}
	oc := Build("opencode", "ordinary", "hello", "/w", "/h", "")
	if oc.Args[0] != "run" || oc.Args[1] != "--auto" {
		t.Fatalf("%+v", oc)
	}
}

func TestEnvOverride(t *testing.T) {
	env := ChildEnv(nil, map[string]string{"GIT_TERMINAL_PROMPT": "1", "FOO": "bar"}, "/hub")
	joined := map[string]string{}
	for _, e := range env {
		for i := 0; i < len(e); i++ {
			if e[i] == '=' {
				joined[e[:i]] = e[i+1:]
				break
			}
		}
	}
	if joined["GIT_TERMINAL_PROMPT"] != "0" || joined["SSH_BATCHMODE"] != "yes" || joined["SSH_ASKPASS"] != "" {
		t.Fatal(joined)
	}
	if joined["IAZIO_DOCS_HUB_PATH"] != "/hub" || joined["FOO"] != "bar" {
		t.Fatal(joined)
	}
}
