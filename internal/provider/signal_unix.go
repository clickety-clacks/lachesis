//go:build !windows

package provider

import (
	"os"
	"syscall"
)

func interruptSignal() os.Signal { return syscall.SIGTERM }
