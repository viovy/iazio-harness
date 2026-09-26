package auth

import "testing"

func TestRequireToken(t *testing.T) {
	if err := RequireInteractiveToken(true, false); err == nil {
		t.Fatal("expected error")
	}
	if err := RequireInteractiveToken(false, false); err != nil {
		t.Fatal(err)
	}
}
