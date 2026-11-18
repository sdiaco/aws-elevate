package term

import (
	"fmt"
	"strings"
	"time"
)

const barWidth = 18

var spinner = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// Progress is one line that a long operation rewrites in place.
type Progress struct {
	u     *UI
	label string
	start time.Time
	frame int
}

// Progress starts a progress line; nothing is printed until the first Update.
func (u *UI) Progress(label string) *Progress {
	return &Progress{u: u, label: label, start: u.now()}
}

func (p *Progress) animated() bool { return p.u.color && p.u.interactive }

// Update rewrites the line with done out of total. Without a terminal it prints nothing:
// a log gets only the result.
func (p *Progress) Update(done, total int) {
	if !p.animated() {
		return
	}
	elapsed := p.u.now().Sub(p.start).Round(time.Second)
	fmt.Fprintf(p.u.out, "\r\x1b[K  %s %s   %s  %d/%d · %s",
		p.u.paint(cyan, string(spinner[p.frame%len(spinner)])), p.label, bar(done, total), done, total, elapsed)
	p.frame++
}

// Done replaces the line with a finished step.
func (p *Progress) Done(label, detail string) {
	if p.animated() {
		fmt.Fprint(p.u.out, "\r\x1b[K")
	}
	p.u.OK(label, detail)
}

func bar(done, total int) string {
	full := 0
	if total > 0 {
		full = min(done, total) * barWidth / total
	}
	return strings.Repeat("█", full) + strings.Repeat("░", barWidth-full)
}
