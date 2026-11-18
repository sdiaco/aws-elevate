package term

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

// fakeUI is a UI on buffers; tty sets what a terminal would set.
func fakeUI(input string, tty bool) (*UI, *bytes.Buffer) {
	out := &bytes.Buffer{}
	u := New(strings.NewReader(input), out, noEnv)
	u.color, u.interactive = tty, tty
	return u, out
}

func TestNoEscapeSequencesWithoutATerminal(t *testing.T) {
	out := &bytes.Buffer{}
	u := New(strings.NewReader(""), out, noEnv)
	u.OK("Account", "111111111111")
	u.Fail("Region", "eu-west-1")
	u.Warn("not on PATH", "move it")
	u.Next("aws elevate ui")
	p := u.Progress("Creating stack")
	p.Update(3, 10)
	p.Done("Stack created", "in 3m")
	if strings.ContainsRune(out.String(), '\x1b') {
		t.Fatalf("escape sequence without a terminal: %q", out.String())
	}
}

func TestNoColorDisablesColors(t *testing.T) {
	noColor := func(k string) string {
		if k == "NO_COLOR" {
			return "1"
		}
		return ""
	}
	if colorWanted(true, noColor) {
		t.Fatal("NO_COLOR=1 must disable colors on a terminal")
	}
	if !colorWanted(true, noEnv) || colorWanted(false, noEnv) {
		t.Fatal("colors only on a terminal without NO_COLOR")
	}
}

func TestColorsOnATerminal(t *testing.T) {
	u, out := fakeUI("", true)
	u.OK("a", "b")
	if !strings.Contains(out.String(), "\x1b[32m✓") {
		t.Fatalf("no green mark: %q", out.String())
	}
	if u.Bold("x") != "\x1b[1mx\x1b[0m" || u.Dim("x") != "\x1b[90mx\x1b[0m" {
		t.Fatal("bold or dim wrong")
	}
}

func TestStepsAreSeparatedByABlankLine(t *testing.T) {
	u, out := fakeUI("", false)
	u.OK("a", "x")
	u.OK("b", "y")
	if out.String() != "  ✓ a   x\n\n  ✓ b   y\n\n" {
		t.Fatalf("got %q", out.String())
	}
}

func TestHeaderWarnNextAndFail(t *testing.T) {
	u, out := fakeUI("", false)
	u.Header("aws-elevate install · v0.0.2")
	u.Fail("Region", "us-east-1")
	u.Warn("aws-elevate is not on your PATH", "move it to ~/.local/bin, or run install.sh")
	u.Next("aws elevate ui", "aws elevate shell")
	want := "aws-elevate install · v0.0.2\n\n" +
		"  ✗ Region   us-east-1\n\n" +
		"  ! aws-elevate is not on your PATH\n    Fix: move it to ~/.local/bin, or run install.sh\n\n" +
		"  Next: aws elevate ui   ·   or: aws elevate shell\n"
	if out.String() != want {
		t.Fatalf("got\n%q\nwant\n%q", out.String(), want)
	}
}

func TestOKWithoutDetail(t *testing.T) {
	u, out := fakeUI("", false)
	u.OK("No open grants", "")
	if out.String() != "  ✓ No open grants\n\n" {
		t.Fatalf("got %q", out.String())
	}
}

func TestAskReturnsTheDefaultOnEmpty(t *testing.T) {
	u, out := fakeUI("\nops@example.com\n", false)
	got, err := u.Ask("Alert e-mail", "none@example.com")
	if err != nil || got != "none@example.com" {
		t.Fatalf("got %q, %v", got, err)
	}
	got, _ = u.Ask("Alert e-mail", "")
	if got != "ops@example.com" {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(out.String(), "  ? Alert e-mail › ") {
		t.Fatalf("prompt: %q", out.String())
	}
}

func TestAskAtTheEndOfInputIsEmpty(t *testing.T) {
	u, _ := fakeUI("", false)
	got, err := u.Ask("Reason", "")
	if err != nil || got != "" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestConfirmWithoutTerminalNeedsYes(t *testing.T) {
	u, _ := fakeUI("y\n", false)
	if _, err := u.Confirm("Proceed?", false); !errors.Is(err, ErrNeedsYes) {
		t.Fatalf("err = %v", err)
	}
	ok, err := u.Confirm("Proceed?", true)
	if !ok || err != nil {
		t.Fatalf("yes: %v, %v", ok, err)
	}
	if line, _ := u.in.ReadString('\n'); line != "y\n" {
		t.Fatalf("Confirm read the input: left %q", line)
	}
}

func TestConfirmDefaultsToNo(t *testing.T) {
	u, out := fakeUI("\ny\nyes\n", true)
	for _, want := range []bool{false, true, true} {
		got, err := u.Confirm("Proceed?", false)
		if err != nil || got != want {
			t.Fatalf("got %v, %v want %v", got, err, want)
		}
	}
	if !strings.Contains(out.String(), "Proceed? [y/N] ") {
		t.Fatalf("prompt: %q", out.String())
	}
}

func TestPlainHasNoColorsAndReadsLines(t *testing.T) {
	out := &bytes.Buffer{}
	u := Plain(strings.NewReader("2\n"), out, true)
	got, err := u.Select("Rule", []Item{{Title: "a"}, {Title: "b"}}, false)
	if err != nil || got != 1 || strings.ContainsRune(out.String(), '\x1b') || !u.Interactive() {
		t.Fatalf("got %d, %v, %q", got, err, out.String())
	}
}
