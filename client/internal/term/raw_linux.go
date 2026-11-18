package term

import "syscall"

const ioctlGet, ioctlSet = syscall.TCGETS, syscall.TCSETS
