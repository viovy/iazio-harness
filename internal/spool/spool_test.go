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
		name        string
		in          string
		max         int
		wantHead    string
		wantTail    string
		wantDropped int
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

func TestDefaultDirAndFlush(t *testing.T) {
	d := DefaultDir()
	if d == "" {
		t.Error("expected non-empty default dir")
	}

	sp := openTestSpool(t)
	_ = sp.AppendRaw("stdout", []byte("hello world\n"))
	events := sp.Flush()
	if len(events) == 0 {
		t.Error("expected events from Flush")
	}

	// 1. toValid
	valid := toValid("hello world")
	if valid != "hello world" {
		t.Errorf("expected clean string, got %s", valid)
	}
	cleaned := toValid("invalid\xff\xfeutf8")
	if !utf8.ValidString(cleaned) {
		t.Errorf("expected valid utf8, got %s", cleaned)
	}

	// 2. Truncate
	h, tail, drop := Truncate("", 10)
	if h != "" || tail != "" || drop != 0 {
		t.Errorf("expected empty truncate on empty string")
	}
	h, tail, drop = Truncate("hello", -1)
	if h != "" || tail != "" || drop != 5 {
		t.Errorf("expected drop all on negative limit")
	}
	h, tail, drop = Truncate("hello world", 100)
	if h != "hello world" || tail != "" || drop != 0 {
		t.Errorf("expected no truncation for short string")
	}
	h, tail, drop = Truncate("start-middle-part-end", 10)
	if drop <= 0 || len(h)+len(tail) > 10 {
		t.Errorf("expected valid truncation, got head=%q tail=%q dropped=%d", h, tail, drop)
	}

	// 3. limit
	if sp.limit() != MaxTextBytes {
		t.Errorf("expected default limit %d, got %d", MaxTextBytes, sp.limit())
	}
	sp.maxText = 1234
	if sp.limit() != 1234 {
		t.Errorf("expected custom limit 1234, got %d", sp.limit())
	}

	// 4. Close when s.f == nil
	emptySpool := &Spool{}
	if err := emptySpool.Close(); err != nil {
		t.Errorf("expected nil from emptySpool.Close, got %v", err)
	}

	// 5. StripANSI
	stripped := StripANSI("\x1b]0;Title\x07Hello \x1b[32mWorld\x1b[0m\r\n")
	if stripped != "Hello World\n" {
		t.Errorf("expected 'Hello World\\n', got %q", stripped)
	}
}

func TestAppendRaw_FilteringAndSync(t *testing.T) {
	sp := openTestSpool(t)
	defer sp.Close()

	if err := sp.AppendRaw("stdout", nil); err != nil {
		t.Fatalf("expected nil for empty byte slice, got: %v", err)
	}

	idleLine := []byte("root agent idle; waiting up to 1h0m0s for 1 background task(s)\n")
	if err := sp.AppendRaw("stdout", idleLine); err != nil {
		t.Fatalf("AppendRaw failed: %v", err)
	}

	mixed := []byte("hello world\nroot agent idle; waiting up to 1h0m0s for 1 background task(s)\ngoodbye\n")
	if err := sp.AppendRaw("stdout", mixed); err != nil {
		t.Fatalf("AppendRaw failed: %v", err)
	}

	if err := sp.Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	got, err := os.ReadFile(sp.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "root agent idle") {
		t.Fatalf("expected filtered idle message, got log:\n%s", string(got))
	}
	if !strings.Contains(string(got), "hello world") || !strings.Contains(string(got), "goodbye") {
		t.Fatalf("expected real lines preserved, got:\n%s", string(got))
	}

	emptySpool := &Spool{}
	if err := emptySpool.Sync(); err != nil {
		t.Fatalf("expected nil Sync on empty spool, got: %v", err)
	}
}

func TestSpool_MultiAttemptAppendPreservesInitialLogs(t *testing.T) {
	dir := t.TempDir()
	jobID := "job-1466"

	// Attempt 1: Spool is opened and writes initial startup telemetry
	sp1, err := Open(dir, jobID)
	if err != nil {
		t.Fatalf("failed to open spool for attempt 1: %v", err)
	}
	attempt1Banner := "\n================================================================================\n" +
		"[iazio] Starting Attempt #1 (trigger_reason: initial_dispatch, previous_exit_code: 0, previous_error: none, conversation_id: 110d238e, timestamp: 2026-10-26T04:05:11Z)\n" +
		"================================================================================\n"
	attempt1Startup := "[ASE-SUPERVISOR] Initializing execution for job job-1466 on /Users/romeo/work/sample-repo (engine: agy, kind: ordinary)\n"
	attempt1ModelOutput := "Model generating code for Attempt 1...\n"
	_ = sp1.AppendRaw("stdout", []byte(attempt1Banner))
	_ = sp1.AppendRaw("stdout", []byte(attempt1Startup))
	_ = sp1.AppendRaw("stdout", []byte(attempt1ModelOutput))
	if err := sp1.Close(); err != nil {
		t.Fatalf("failed to close attempt 1 spool: %v", err)
	}

	// Attempt 2: Re-open spool for the SAME job ID (re-dispatch / retry / resume)
	sp2, err := Open(dir, jobID)
	if err != nil {
		t.Fatalf("failed to open spool for attempt 2: %v", err)
	}
	attempt2Banner := "\n================================================================================\n" +
		"[iazio] Starting Attempt #2 (trigger_reason: lease_expired, previous_exit_code: 1, previous_error: LEASE_EXPIRED, conversation_id: 110d238e, timestamp: 2026-10-26T04:18:46Z)\n" +
		"================================================================================\n"
	attempt2Startup := "[ASE-SUPERVISOR] Initializing execution for job job-1466 on /Users/romeo/work/sample-repo (engine: agy, kind: resume)\n"
	_ = sp2.AppendRaw("stdout", []byte(attempt2Banner))
	_ = sp2.AppendRaw("stdout", []byte(attempt2Startup))
	if err := sp2.Close(); err != nil {
		t.Fatalf("failed to close attempt 2 spool: %v", err)
	}

	// Verify disk log: Attempt 1 logs MUST NOT be wiped to 0 bytes
	content, err := os.ReadFile(sp2.LogPath())
	if err != nil {
		t.Fatalf("failed to read disk log: %v", err)
	}
	logStr := string(content)

	if !strings.Contains(logStr, "[iazio] Starting Attempt #1") {
		t.Errorf("Attempt 1 banner missing from disk log")
	}
	if !strings.Contains(logStr, "[ASE-SUPERVISOR] Initializing execution for job job-1466 on /Users/romeo/work/sample-repo (engine: agy, kind: ordinary)") {
		t.Errorf("Attempt 1 startup log missing from disk log")
	}
	if !strings.Contains(logStr, "Model generating code for Attempt 1...") {
		t.Errorf("Attempt 1 output missing from disk log")
	}
	if !strings.Contains(logStr, "[iazio] Starting Attempt #2") {
		t.Errorf("Attempt 2 banner missing from disk log")
	}
	if !strings.Contains(logStr, "kind: resume") {
		t.Errorf("Attempt 2 startup log missing from disk log")
	}
}
