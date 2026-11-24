package term

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

var rules = []Item{
	{Title: "db-prod-read", Detail: "RDS read-only"},
	{Title: "ssm-prod", Detail: "SSM sessions"},
	{Title: "s3-audit", Detail: "S3 read"},
}

// fakeRaw counts how often the terminal was restored.
type fakeRaw struct {
	fail     error
	restored int
}

func (f *fakeRaw) enter(int) (func() error, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return func() error { f.restored++; return nil }, nil
}

func rawUI(input string) (*UI, *fakeRaw) {
	u, _ := fakeUI(input, true)
	r := &fakeRaw{}
	u.raw = r
	return u, r
}

func TestSelectByNumberOrName(t *testing.T) {
	for input, want := range map[string]int{"2\n": 1, "ssm-prod\n": 1, "3": 2} {
		u, out := fakeUI(input, false)
		got, err := u.Select("Rule", rules, false)
		if err != nil || got != want {
			t.Fatalf("%q: got %d, %v", input, got, err)
		}
		if !strings.Contains(out.String(), "    1) db-prod-read   RDS read-only\n") {
			t.Fatalf("list: %q", out.String())
		}
	}
	for _, input := range []string{"x\n", "4\n", "\n"} {
		u, _ := fakeUI(input, false)
		_, err := u.Select("Rule", rules, false)
		if err == nil || !strings.Contains(err.Error(), "no choice named") {
			t.Fatalf("%q: err = %v", input, err)
		}
	}
}

func TestSelectOptionalAcceptsEmpty(t *testing.T) {
	u, out := fakeUI("\n", false)
	got, err := u.Select("Auditor group", rules, true)
	if err != nil || got != -1 {
		t.Fatalf("got %d, %v", got, err)
	}
	if !strings.Contains(out.String(), "empty for none") {
		t.Fatalf("prompt: %q", out.String())
	}
}

func TestSelectWithArrowsAndFilter(t *testing.T) {
	cases := map[string]int{
		"\x1b[B\r":             1, // down
		"\x1b[B\x1b[B\x1b[A\r": 1, // down, down, up
		"\x1b[A\r":             0, // up at the top stays
		"ssm\r":                1, // filter
		"S3\r":                 2, // filter ignores case
		"ssx\x7f\x7fsm\r":      1, // backspace
		"zz\r\x7f\x7f\r":       0, // enter on no match does nothing
		"\n":                   0, // newline works as enter
	}
	for input, want := range cases {
		u, r := rawUI(input)
		got, err := u.Select("Rule", rules, false)
		if err != nil || got != want {
			t.Fatalf("%q: got %d, %v", input, got, err)
		}
		if r.restored != 1 {
			t.Fatalf("%q: restored %d times", input, r.restored)
		}
	}
}

func TestSelectWithArrowsOptionalNone(t *testing.T) {
	u, _ := rawUI("\x1b[B\x1b[B\x1b[B\r")
	got, err := u.Select("Auditor group", rules, true)
	if err != nil || got != -1 {
		t.Fatalf("got %d, %v", got, err)
	}
}

func TestSelectShowsTheChoice(t *testing.T) {
	u, _ := rawUI("\x1b[B\r")
	u.color = false
	out := u.out.(interface{ String() string })
	if _, err := u.Select("Rule", rules, false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), "  ? Rule › ssm-prod\n") {
		t.Fatalf("final line: %q", out.String())
	}
}

func TestSelectRestoresTheTerminalOnInterrupt(t *testing.T) {
	u, r := rawUI("\x1b[B\x03")
	_, err := u.Select("Rule", rules, false)
	if !errors.Is(err, context.Canceled) || r.restored != 1 {
		t.Fatalf("err = %v, restored %d", err, r.restored)
	}

	u, r = rawUI("")
	u.in.Reset(errReader{io.ErrUnexpectedEOF})
	_, err = u.Select("Rule", rules, false)
	if !errors.Is(err, io.ErrUnexpectedEOF) || r.restored != 1 {
		t.Fatalf("err = %v, restored %d", err, r.restored)
	}
}

func TestSelectFallsBackWhenRawFails(t *testing.T) {
	u, out := fakeUI("2\n", true)
	u.raw = &fakeRaw{fail: errors.New("not a terminal")}
	got, err := u.Select("Rule", rules, false)
	if err != nil || got != 1 {
		t.Fatalf("got %d, %v", got, err)
	}
	if !strings.Contains(out.String(), "1) db-prod-read") {
		t.Fatalf("no numbered list: %q", out.String())
	}
}

func TestSelectWithoutItems(t *testing.T) {
	u, _ := fakeUI("", true)
	if _, err := u.Select("Rule", nil, false); err == nil {
		t.Fatal("no error without choices")
	}
	if got, err := u.Select("Auditor", nil, true); err != nil || got != -1 {
		t.Fatalf("optional without choices: %d, %v", got, err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// many returns n items named item-01, item-02, …
func many(n int) []Item {
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{Title: fmt.Sprintf("item-%02d", i+1)}
	}
	return items
}

// lastFrame is the last drawing of the choices, before Ctrl-C erased it.
func lastFrame(out string) string {
	out = out[:strings.LastIndex(out, "\x1b[")] // the erase on Ctrl-C
	if i := strings.LastIndex(out, "\x1b[J"); i >= 0 {
		return out[i+len("\x1b[J"):]
	}
	return out
}

func TestSelectShowsAtMostEightRowsAndCountsTheHiddenOnes(t *testing.T) {
	u, _ := rawUI("\x03")
	u.color = false
	out := u.out.(interface{ String() string })
	_, _ = u.Select("Account", many(20), false)
	frame := out.String()
	for _, want := range []string{"item-01", "item-08", "↓ 12 more"} {
		if !strings.Contains(frame, want) {
			t.Errorf("missing %q in %q", want, frame)
		}
	}
	for _, absent := range []string{"item-09", "↑ "} {
		if strings.Contains(frame, absent) {
			t.Errorf("unexpected %q in %q", absent, frame)
		}
	}
}

func TestSelectScrollsWithTheCursor(t *testing.T) {
	down := strings.Repeat("\x1b[B", 10) // cursor on item-11
	u, _ := rawUI(down + "\x03")
	u.color = false
	out := u.out.(interface{ String() string })
	_, _ = u.Select("Account", many(20), false)
	frame := lastFrame(out.String())
	for _, want := range []string{"↑ 3 more", "item-04", "item-11", "↓ 9 more"} {
		if !strings.Contains(frame, want) {
			t.Errorf("missing %q in %q", want, frame)
		}
	}
	if strings.Contains(frame, "item-03") || strings.Contains(frame, "item-12") {
		t.Errorf("rows outside the window in %q", frame)
	}
}

func TestSelectPicksARowBeyondTheFirstWindow(t *testing.T) {
	u, _ := rawUI(strings.Repeat("\x1b[B", 14) + "\r")
	got, err := u.Select("Account", many(20), false)
	if err != nil || got != 14 {
		t.Fatalf("got %d, %v", got, err)
	}
}

func TestSelectFilterNarrowsBeforeTheWindow(t *testing.T) {
	u, _ := rawUI("item-1\x03")
	u.color = false
	out := u.out.(interface{ String() string })
	_, _ = u.Select("Account", many(20), false)
	frame := lastFrame(out.String())
	// item-10 … item-19 match: ten rows, eight shown, two below.
	for _, want := range []string{"item-10", "item-17", "↓ 2 more"} {
		if !strings.Contains(frame, want) {
			t.Errorf("missing %q in %q", want, frame)
		}
	}
	if strings.Contains(frame, "item-01") {
		t.Errorf("filtered-out row in %q", frame)
	}
}
