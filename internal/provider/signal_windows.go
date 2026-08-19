//go:build windows

package provider

import "os"

func interruptSignal() os.Signal { return os.Interrupt }
