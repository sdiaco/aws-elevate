package term

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Item is one choice of a Select.
type Item struct{ Title, Detail string }

const noneTitle = "(none)"

// Select asks for one of items and returns its index; -1 is "none", only when optional.
// On a terminal the arrows move and typing filters; otherwise, or when the terminal cannot
// switch to single keys, the answer is a number or a name of a numbered list.
func (u *UI) Select(label string, items []Item, optional bool) (int, error) {
	if len(items) == 0 {
		if optional {
			return -1, nil
		}
		return 0, fmt.Errorf("no choice for %s", strings.ToLower(label))
	}
	if u.interactive && u.raw != nil {
		if restore, err := u.raw.enter(u.fd); err == nil {
			return u.selectKeys(label, items, optional, restore)
		}
	}
	return u.selectLine(label, items, optional)
}

func (u *UI) selectLine(label string, items []Item, optional bool) (int, error) {
	fmt.Fprintf(u.out, "  %s %s\n", u.paint(cyan, "?"), label)
	for i, it := range items {
		fmt.Fprintf(u.out, "    %d) %s", i+1, it.Title)
		if it.Detail != "" {
			fmt.Fprintf(u.out, "   %s", u.Dim(it.Detail))
		}
		fmt.Fprintln(u.out)
	}
	question := "    number or name › "
	if optional {
		question = "    number or name, empty for none › "
	}
	fmt.Fprint(u.out, question)
	answer, err := u.readLine()
	if err != nil {
		return 0, err
	}
	if answer == "" && optional {
		return -1, nil
	}
	if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(items) {
		return n - 1, nil
	}
	for i, it := range items {
		if it.Title == answer {
			return i, nil
		}
	}
	return 0, fmt.Errorf("no choice named %q", answer)
}

// selectKeys reads single keys. Ctrl-C arrives as a byte because the raw mode turns signals
// off, so the terminal is always restored here and never left raw.
func (u *UI) selectKeys(label string, items []Item, optional bool, restore func() error) (idx int, err error) {
	defer func() {
		if rerr := restore(); rerr != nil && err == nil {
			err = fmt.Errorf("restore the terminal: %w", rerr)
		}
	}()
	all := make([]int, len(items))
	for i := range items {
		all[i] = i
	}
	if optional {
		all = append(all, -1)
	}
	filter, cursor, drawn := "", 0, 0
	for {
		shown := matching(items, all, filter)
		if cursor >= len(shown) {
			cursor = max(len(shown)-1, 0)
		}
		drawn = u.drawChoices(label, items, shown, cursor, filter, drawn)
		b, rerr := u.in.ReadByte()
		if rerr != nil {
			return 0, fmt.Errorf("read the answer: %w", rerr)
		}
		switch b {
		case 3: // Ctrl-C
			u.clear(drawn)
			return 0, context.Canceled
		case '\r', '\n':
			if len(shown) == 0 {
				continue
			}
			u.clear(drawn)
			title := noneTitle
			if shown[cursor] >= 0 {
				title = items[shown[cursor]].Title
			}
			fmt.Fprintf(u.out, "%s%s\n", u.prompt(label), title)
			return shown[cursor], nil
		case 0x7f, 8: // Backspace
			if filter != "" {
				filter = filter[:len(filter)-1]
			}
		case 0x1b:
			if err := u.arrow(&cursor, len(shown)); err != nil {
				return 0, err
			}
		default:
			if b >= ' ' && b < 0x7f {
				filter += string(b)
				cursor = 0
			}
		}
	}
}

// arrow reads the rest of an arrow key, ESC [ A or ESC [ B, and moves the cursor.
func (u *UI) arrow(cursor *int, n int) error {
	seq := make([]byte, 2)
	for i := range seq {
		b, err := u.in.ReadByte()
		if err != nil {
			return fmt.Errorf("read the answer: %w", err)
		}
		seq[i] = b
	}
	switch string(seq) {
	case "[A":
		if *cursor > 0 {
			*cursor--
		}
	case "[B":
		if *cursor < n-1 {
			*cursor++
		}
	}
	return nil
}

func matching(items []Item, all []int, filter string) []int {
	if filter == "" {
		return all
	}
	f := strings.ToLower(filter)
	var out []int
	for _, i := range all {
		title := noneTitle
		if i >= 0 {
			title = items[i].Title
		}
		if strings.Contains(strings.ToLower(title), f) {
			out = append(out, i)
		}
	}
	return out
}

// drawChoices replaces the previous drawing and returns how many lines it wrote.
func (u *UI) drawChoices(label string, items []Item, shown []int, cursor int, filter string, drawn int) int {
	u.clear(drawn)
	hint := filter
	if hint == "" {
		hint = u.Dim("↑↓ to move, type to filter")
	}
	fmt.Fprintf(u.out, "%s%s\r\n", u.prompt(label), hint)
	for i, idx := range shown {
		mark, title, detail := "  ", noneTitle, ""
		if idx >= 0 {
			title, detail = items[idx].Title, items[idx].Detail
		}
		if i == cursor {
			mark, title = u.paint(cyan, "❯ "), u.Bold(title)
		}
		line := "  " + mark + title
		if detail != "" {
			line += "   " + u.Dim(detail)
		}
		fmt.Fprintf(u.out, "%s\r\n", line)
	}
	if len(shown) == 0 {
		fmt.Fprintf(u.out, "    %s\r\n", u.Dim("no match"))
		return 2
	}
	return len(shown) + 1
}

// clear erases the last n lines written, leaving the cursor where they began.
func (u *UI) clear(n int) {
	if n > 0 {
		fmt.Fprintf(u.out, "\x1b[%dA\r\x1b[J", n)
	}
}

// errNotTerminal is what the raw mode answers where it is not supported.
var errNotTerminal = errors.New("single keys are not supported on this terminal")
