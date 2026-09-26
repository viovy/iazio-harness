package spool

import (
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestStripANSI(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "color", in: "\x1b[31mred\x1b[0m", want: "red"},
		{name: "cursor", in: "\x1b[2K\x1b[1Ghi", want: "hi"},
		{name: "osc", in: "\x1b]0;title\x07body", want: "body"},
		{name: "cr", in: "foo\rbar", want: "foobar"},
		{name: "plain", in: "hello", want: "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripANSI(tt.in)
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
			if strings.Contains(got, "\x1b") {
				t.Fatal(got)
			}
		})
	}
}

func TestUTF8Truncation(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		max          int
		wantHead     string
		wantTail     string
		wantDropped  int
	}{
		{name: "fits", in: "abc", max: 4, wantHead: "abc", wantTail: "", wantDropped: 0},
		{name: "ascii middle", in: "abcdef", max: 4, wantHead: "ab", wantTail: "ef", wantDropped: 2},
		{name: "multibyte boundary", in: "a\u00e9b", max: 3, wantHead: "a", wantTail: "b", wantDropped: 2},
		{name: "rune does not fit head", in: "\u00e9\u00e9", max: 3, wantHead: "", wantTail: "\u00e9", wantDropped: 2},
		{name: "three runes", in: "\u00e9\u00e9\u00e9", max: 4, wantHead: "\u00e9", wantTail: "\u00e9", wantDropped: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			head, tail, dropped := Truncate(tt.in, tt.max)
			if head != tt.wantHead || tail != tt.wantTail || dropped != tt.wantDropped {
				t.Fatalf("head=%q tail=%q dropped=%d", head, tail, dropped)
			}
			if !utf8.ValidString(head) || !utf8.ValidString(tail) {
				t.Fatal("cut inside a rune")
			}
			if len(head)+len(tail) > tt.max {
				t.Fatalf("kept %d max %d", len(head)+len(tail), tt.max)
			}
		})
	}
}

func TestFlushCapAndSeq(t *testing.T) {
	sp := openTestSpool(t)
	over := strings.Repeat("a", MaxTextBytes+1)
	if err := sp.AppendRaw("stdout", []byte(over)); err != nil {
		t.Fatal(err)
	}
	ev := sp.FlushAt(time.Unix(1_700_000_000, 0))
	var trunc *Event
	var text int
	for i := range ev {
		if ev[i].Seq < 1 {
			t.Fatalf("seq %d", ev[i].Seq)
		}
		if ev[0].Seq != 1 {
			t.Fatalf("first seq %d", ev[0].Seq)
		}
		text += len(ev[i].Text) + len(ev[i].Head) + len(ev[i].Tail)
		if ev[i].Type == EventStreamTruncated {
			trunc = &ev[i]
		}
		if !utf8.ValidString(ev[i].Text) || !utf8.ValidString(ev[i].Head) || !utf8.ValidString(ev[i].Tail) {
			t.Fatal("invalid utf-8")
		}
	}
	if trunc == nil || trunc.DroppedBytes != 1 {
		t.Fatalf("truncation %+v", trunc)
	}
	if len(trunc.Head)+len(trunc.Tail) > MaxTextBytes {
		t.Fatalf("kept %d", len(trunc.Head)+len(trunc.Tail))
	}
	if text/2 > MaxTextBytes {
		t.Fatal("posted text over the cap")
	}
}

func TestRawLogKeepsANSI(t *testing.T) {
	sp := openTestSpool(t)
	raw := []byte("\x1b[31mred\x1b[0m")
	if err := sp.AppendRaw("stdout", raw); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(sp.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatalf("log %q", got)
	}
	ev := sp.FlushAt(time.Now())
	if len(ev) != 1 || ev[0].Type != EventOutputChunk || ev[0].Seq != 1 || ev[0].Text != "red" {
		t.Fatalf("%+v", ev)
	}
}

func TestOutputTick(t *testing.T) {
	sp := openTestSpool(t)
	base := time.Unix(1_700_000_100, 0)
	sp.opened = base
	sp.lastByte = time.Time{}
	ev := sp.FlushAt(base.Add(1500 * time.Millisecond))
	if len(ev) != 1 || ev[0].Type != EventOutputTick || ev[0].SilentForMs != 1500 {
		t.Fatalf("%+v", ev)
	}
	if ev[0].Seq != 0 {
		t.Fatal(ev[0].Seq)
	}
}

func TestCloseSpool(t *testing.T) {
	sp := openTestSpool(t)
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sp.AppendRaw("stdout", []byte("x")); err == nil {
		t.Fatal("spool stayed open after the child exited")
	}
}

func TestSpoolDirMode(t *testing.T) {
	if DirMode != 0o755 {
		t.Fatalf("mode %o", DirMode)
	}
	dir := t.TempDir() + "/nested/spool"
	sp, err := Open(dir, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatal("spool dir missing")
	}
}

func openTestSpool(t *testing.T) *Spool {
	t.Helper()
	sp, err := Open(t.TempDir(), "job-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	return sp
}
