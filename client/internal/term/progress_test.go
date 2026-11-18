package term

import (
	"strings"
	"testing"
	"time"
)

func TestProgressRewritesOneLine(t *testing.T) {
	u, out := fakeUI("", true)
	start := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	now := start
	u.now = func() time.Time { return now }
	p := u.Progress("Creating stack")
	now = start.Add(130 * time.Second)
	p.Update(41, 68)
	line := out.String()
	for _, want := range []string{"\r\x1b[K", "Creating stack", "41/68", "2m10s", "█", "░"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("progress wrote a new line: %q", line)
	}
	p.Done("Stack created", "in 3m42s")
	if !strings.HasSuffix(out.String(), "\r\x1b[K  \x1b[32m✓\x1b[0m Stack created   in 3m42s\n\n") {
		t.Fatalf("done: %q", out.String())
	}
}

func TestProgressBar(t *testing.T) {
	for _, c := range []struct {
		done, total int
		full        int
	}{{0, 10, 0}, {5, 10, 9}, {10, 10, 18}, {12, 10, 18}, {3, 0, 0}} {
		got := bar(c.done, c.total)
		if strings.Count(got, "█") != c.full || len([]rune(got)) != barWidth {
			t.Fatalf("bar(%d,%d) = %q", c.done, c.total, got)
		}
	}
}

func TestProgressWithoutTerminalPrintsOnlyTheResult(t *testing.T) {
	u, out := fakeUI("", false)
	p := u.Progress("Creating stack")
	p.Update(1, 2)
	p.Update(2, 2)
	p.Done("Stack created", "")
	if out.String() != "  ✓ Stack created\n\n" {
		t.Fatalf("got %q", out.String())
	}
}
