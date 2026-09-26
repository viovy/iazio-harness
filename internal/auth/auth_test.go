package auth

import "testing"

func TestGateBeforeCLI(t *testing.T) {
	t.Setenv("IAZIO_HARNESS_CONFIG", t.TempDir()+"/missing.json")
	t.Setenv("IAZIO_HARNESS_NONINTERACTIVE", "1")
	t.Setenv("INVOCATION_ID", "")
	if err := GateBeforeCLI(); err == nil {
		t.Fatal("expected missing refresh token")
	}
}
