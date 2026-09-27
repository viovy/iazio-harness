// Package spool stores raw child bytes and emits capped, ANSI-free UTF-8 events.
package spool

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Event type names posted for one job.
const (
	EventOutputChunk     = "OUTPUT_CHUNK"
	EventStreamTruncated = "STREAM_TRUNCATED"
	EventOutputTick      = "OUTPUT_TICK"
)

// DirMode is the mode passed to MkdirAll for the spool directory.
const DirMode os.FileMode = 0o755

// MaxTextBytes is the maximum stripped text posted for one job in one second.
const MaxTextBytes = 64 * 1024

// Event is one flushed output record.
// Schedule environment is not copied onto this struct.
type Event struct {
	Type         string `json:"type"`
	Stream       string `json:"stream,omitempty"`
	Seq          int    `json:"seq,omitempty"`
	Text         string `json:"text,omitempty"`
	Head         string `json:"head,omitempty"`
	Tail         string `json:"tail,omitempty"`
	DroppedBytes int    `json:"dropped_bytes,omitempty"`
	SilentForMs  int64  `json:"silent_for_ms,omitempty"`
}

// Spool appends raw child bytes and flushes stripped text.
type Spool struct {
	mu          sync.Mutex
	dir         string
	jobID       string
	f           *os.File
	bufs        map[string]*bytes.Buffer
	seq         int
	opened      time.Time
	lastByte    time.Time
	windowStart time.Time
	windowBytes int
	maxText     int
	closed      bool
}

// DefaultDir returns ~/.iazio/spool.
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".iazio", "spool")
	}
	return filepath.Join(home, ".iazio", "spool")
}

// Open creates the spool directory with mode 0755 and the raw log file.
func Open(dir, jobID string) (*Spool, error) {
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, jobID+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return &Spool{
		dir:    dir,
		jobID:  jobID,
		f:      f,
		bufs:   map[string]*bytes.Buffer{},
		seq:    1,
		opened: now,
	}, nil
}

// LogPath returns the raw log path.
func (s *Spool) LogPath() string {
	return filepath.Join(s.dir, s.jobID+".log")
}

// AppendRaw writes unmodified bytes, including ANSI, to the log and the flush buffer.
func (s *Spool) AppendRaw(stream string, p []byte) error {
	if len(p) == 0 {
		return nil
	}
	if stream == "" {
		stream = "stdout"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.f == nil {
		return os.ErrClosed
	}
	if _, err := s.f.Write(p); err != nil {
		return err
	}
	buf := s.bufs[stream]
	if buf == nil {
		buf = &bytes.Buffer{}
		s.bufs[stream] = buf
	}
	_, _ = buf.Write(p)
	s.lastByte = time.Now()
	return nil
}

// FlushAt strips ANSI from buffered bytes and returns events for this instant.
// OUTPUT_CHUNK sequence numbers start at 1. At most MaxTextBytes of text are emitted per second.
// OUTPUT_TICK is returned when there are no new bytes.
func (s *Spool) FlushAt(now time.Time) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seq == 0 {
		s.seq = 1
	}
	if !s.hasBytes() {
		base := s.lastByte
		if base.IsZero() {
			base = s.opened
		}
		ms := now.Sub(base).Milliseconds()
		if ms < 0 {
			ms = 0
		}
		return []Event{{Type: EventOutputTick, SilentForMs: ms}}
	}
	limit := s.limit()
	if s.windowStart.IsZero() || now.Sub(s.windowStart) >= time.Second {
		s.windowStart = now
		s.windowBytes = 0
	}
	remaining := limit - s.windowBytes
	if remaining < 0 {
		remaining = 0
	}
	var events []Event
	for _, stream := range []string{"stdout", "stderr"} {
		buf := s.bufs[stream]
		if buf == nil || buf.Len() == 0 {
			continue
		}
		raw := buf.String()
		buf.Reset()
		text := toValid(StripANSI(raw))
		ev, used := s.emitText(stream, text, remaining)
		events = append(events, ev...)
		remaining -= used
		if remaining < 0 {
			remaining = 0
		}
		s.windowBytes += used
	}
	if len(events) == 0 {
		return []Event{{Type: EventOutputTick, SilentForMs: 0}}
	}
	return events
}

func (s *Spool) emitText(stream, text string, remaining int) ([]Event, int) {
	if text == "" {
		return nil, 0
	}
	if !utf8.ValidString(text) {
		text = toValid(text)
	}
	if len(text) <= remaining {
		ev := Event{Type: EventOutputChunk, Stream: stream, Seq: s.takeSeq(), Text: text}
		return []Event{ev}, len(text)
	}
	head, tail, dropped := Truncate(text, remaining)
	var events []Event
	used := 0
	if head != "" {
		events = append(events, Event{Type: EventOutputChunk, Stream: stream, Seq: s.takeSeq(), Text: head})
		used += len(head)
	}
	events = append(events, Event{
		Type:         EventStreamTruncated,
		Stream:       stream,
		Seq:          s.takeSeq(),
		Head:         head,
		Tail:         tail,
		DroppedBytes: dropped,
	})
	if tail != "" {
		events = append(events, Event{Type: EventOutputChunk, Stream: stream, Seq: s.takeSeq(), Text: tail})
		used += len(tail)
	}
	return events, used
}

func (s *Spool) takeSeq() int {
	n := s.seq
	s.seq++
	return n
}

func (s *Spool) hasBytes() bool {
	for _, buf := range s.bufs {
		if buf != nil && buf.Len() > 0 {
			return true
		}
	}
	return false
}

func (s *Spool) limit() int {
	if s.maxText > 0 {
		return s.maxText
	}
	return MaxTextBytes
}

// Close closes the raw log. Call it when the child exits.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

func toValid(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "")
}

var (
	ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	oscPattern  = regexp.MustCompile(`\x1b\][^\x07]*(?:\x07|\x1b\\)`)
)

// StripANSI removes cursor-control sequences and carriage returns from a chunk of text.
func StripANSI(s string) string {
	s = oscPattern.ReplaceAllString(s, "")
	s = ansiPattern.ReplaceAllString(s, "")
	return strings.ReplaceAll(s, "\r", "")
}

// Truncate keeps a rune-safe head and tail that together fit in limit bytes.
// Bytes that do not fit a rune boundary stay in the dropped middle.
func Truncate(s string, limit int) (head, tail string, dropped int) {
	if limit <= 0 || s == "" {
		return "", "", len(s)
	}
	if len(s) <= limit {
		return s, "", 0
	}
	head = cutStart(s, limit/2)
	tail = cutEnd(s, limit-len(head))
	if len(head)+len(tail) > len(s) {
		return s, "", 0
	}
	// Head and tail must not overlap.
	if len(head) > 0 && len(s)-len(tail) < len(head) {
		tail = cutEnd(s[len(head):], limit-len(head))
	}
	dropped = len(s) - len(head) - len(tail)
	if dropped < 0 {
		dropped = 0
	}
	return head, tail, dropped
}

func cutStart(s string, n int) string {
	if n > len(s) {
		n = len(s)
	}
	for n > 0 && !utf8.ValidString(s[:n]) {
		n--
	}
	return s[:n]
}

func cutEnd(s string, n int) string {
	if n > len(s) {
		return s
	}
	i := len(s) - n
	if i < 0 {
		i = 0
	}
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}
