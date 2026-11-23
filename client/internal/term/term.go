// Package term prints the steps of a command and asks its questions: one style for every
// command, with colors and animations only on an interactive terminal.
package term

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// ErrNeedsYes stops a command that needs a confirmation when nobody can type it.
var ErrNeedsYes = errors.New("run it again with --yes to confirm without a terminal")

const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	red    = "\x1b[31m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	cyan   = "\x1b[36m"
	grey   = "\x1b[90m"
)

// UI writes to one output and reads answers from one input. A command creates it once:
// the input is buffered, so a second UI on the same input could lose typed answers.
type UI struct {
	out         io.Writer
	in          *bufio.Reader
	fd          int  // input file descriptor for the raw mode, -1 when not a file
	color       bool // colors and animations
	interactive bool // someone can answer: input and output are terminals
	raw         rawMode
	now         func() time.Time
}

// rawMode switches the terminal to reading single keys; restore puts it back as it was.
type rawMode interface {
	enter(fd int) (restore func() error, err error)
}

// New decides colors and interaction from the input and output: both terminals means
// interactive; colors need a terminal output and an empty NO_COLOR.
func New(in io.Reader, out io.Writer, getenv func(string) string) *UI {
	fd := -1
	if f, ok := in.(*os.File); ok {
		fd = int(f.Fd())
	}
	outTTY := isTerminal(out)
	return &UI{
		out:         out,
		in:          bufio.NewReader(in),
		fd:          fd,
		color:       colorWanted(outTTY, getenv),
		interactive: outTTY && isTerminal(in),
		raw:         sysRaw{},
		now:         time.Now,
	}
}

func colorWanted(outTTY bool, getenv func(string) string) bool {
	return outTTY && getenv("NO_COLOR") == ""
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// Interactive tells whether someone can answer a question.
func (u *UI) Interactive() bool { return u.interactive }

// Writer is the output, for tables and text that are not steps.
func (u *UI) Writer() io.Writer { return u.out }

func (u *UI) paint(code, s string) string {
	if !u.color {
		return s
	}
	return code + s + reset
}

// Bold marks what the reader looks for: ids, accounts, expiry times.
func (u *UI) Bold(s string) string { return u.paint(bold, s) }

// Dim marks details.
func (u *UI) Dim(s string) string { return u.paint(grey, s) }

// Header is the first line of a command.
func (u *UI) Header(title string) {
	fmt.Fprintf(u.out, "%s\n\n", u.Bold(title))
}

func (u *UI) step(mark, label, detail string) {
	if detail == "" {
		fmt.Fprintf(u.out, "  %s %s\n\n", mark, label)
		return
	}
	fmt.Fprintf(u.out, "  %s %s   %s\n\n", mark, label, detail)
}

// OK prints a step that succeeded.
func (u *UI) OK(label, detail string) { u.step(u.paint(green, "✓"), label, detail) }

// Fail prints a step that failed.
func (u *UI) Fail(label, detail string) { u.step(u.paint(red, "✗"), label, detail) }

// Warn prints something the user should know, with how to fix it.
func (u *UI) Warn(text string, fix ...string) {
	fmt.Fprintf(u.out, "  %s %s\n", u.paint(yellow, "!"), text)
	for _, f := range fix {
		fmt.Fprintf(u.out, "    Fix: %s\n", f)
	}
	fmt.Fprintln(u.out)
}

// Next is the last line of a command that went well: what to run now.
func (u *UI) Next(commands ...string) {
	if len(commands) == 0 {
		return
	}
	fmt.Fprintf(u.out, "  Next: %s\n", strings.Join(commands, "   ·   or: "))
}

func (u *UI) prompt(label string) string {
	return fmt.Sprintf("  %s %s › ", u.paint(cyan, "?"), label)
}

// readLine reads one answer; the end of the input is an empty answer.
func (u *UI) readLine() (string, error) {
	line, err := u.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read the answer: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// Ask reads a line; an empty answer is def.
func (u *UI) Ask(label, def string) (string, error) {
	p := u.prompt(label)
	if def != "" {
		p += u.Dim("(" + def + ") ")
	}
	fmt.Fprint(u.out, p)
	answer, err := u.readLine()
	if err != nil {
		return "", err
	}
	if answer == "" {
		return def, nil
	}
	return answer, nil
}

// Confirm asks a yes or no question, no by default. yes answers it without asking; without
// a terminal and without yes the command stops with ErrNeedsYes.
func (u *UI) Confirm(question string, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	if !u.interactive {
		return false, ErrNeedsYes
	}
	fmt.Fprintf(u.out, "  %s %s [y/N] ", u.paint(cyan, "?"), question)
	answer, err := u.readLine()
	if err != nil {
		return false, err
	}
	answer = strings.ToLower(answer)
	return answer == "y" || answer == "yes", nil
}

// ConfirmYes is Confirm with yes as the default: an empty answer is yes, only n or no is no.
func (u *UI) ConfirmYes(question string, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	if !u.interactive {
		return false, ErrNeedsYes
	}
	fmt.Fprintf(u.out, "  %s %s [Y/n] ", u.paint(cyan, "?"), question)
	answer, err := u.readLine()
	if err != nil {
		return false, err
	}
	answer = strings.ToLower(answer)
	return answer != "n" && answer != "no", nil
}

// Plain is a UI without colors that reads answers line by line: interactive says whether
// it behaves as a terminal with someone typing. For tests of the commands.
func Plain(in io.Reader, out io.Writer, interactive bool) *UI {
	return &UI{out: out, in: bufio.NewReader(in), fd: -1, interactive: interactive, now: time.Now}
}

// Colored is Plain with colors, for tests of what is highlighted.
func Colored(in io.Reader, out io.Writer, interactive bool) *UI {
	u := Plain(in, out, interactive)
	u.color = true
	return u
}
