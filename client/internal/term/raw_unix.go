//go:build linux || darwin

package term

import (
	"syscall"
	"unsafe"
)

// sysRaw reads single keys with the terminal's own settings, through ioctl: echo, line
// editing and the signal keys go off, so Ctrl-C arrives as a key and the terminal is
// always restored by the code that turned them off.
type sysRaw struct{}

func (sysRaw) enter(fd int) (func() error, error) {
	var old syscall.Termios
	if err := ioctl(fd, ioctlGet, &old); err != nil {
		return nil, err
	}
	t := old
	t.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.ISIG | syscall.IEXTEN
	t.Iflag &^= syscall.ICRNL | syscall.IXON
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	if err := ioctl(fd, ioctlSet, &t); err != nil {
		return nil, err
	}
	return func() error { return ioctl(fd, ioctlSet, &old) }, nil
}

func ioctl(fd int, req uintptr, t *syscall.Termios) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(t))); errno != 0 {
		return errno
	}
	return nil
}
