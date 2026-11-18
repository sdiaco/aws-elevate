//go:build !linux && !darwin

package term

// sysRaw is not supported here: Select falls back to a numbered list.
type sysRaw struct{}

func (sysRaw) enter(int) (func() error, error) { return nil, errNotTerminal }
