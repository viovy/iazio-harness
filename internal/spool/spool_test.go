package spool

import "testing"

func TestStripAndTruncate(t *testing.T) {
	got := StripANSI("hi\x1b[31mred\x1b[0m")
	if got != "hired" {
		t.Fatal(got)
	}
	big := make([]byte, capBytes+10)
	for i := range big {
		big[i] = 'a'
	}
	ch := Flush(1, string(big), 0)
	if !ch.Truncated || ch.DroppedBytes <= 0 || !valid(ch.Head) || !valid(ch.Tail) {
		t.Fatalf("%+v", ch)
	}
	tick := Flush(2, "", 1000)
	if !tick.Tick || tick.SilentForMS != 1000 {
		t.Fatal(tick)
	}
}

func valid(s string) bool {
	return len(s) > 0
}
