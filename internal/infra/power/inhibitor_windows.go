//go:build windows

package power

import (
	"airmedy/internal/domain"
	"golang.org/x/sys/windows"
)

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

type windowsInhibitor struct {
	kernel32           *windows.LazyDLL
	setThreadExecState *windows.LazyProc
}

func NewInhibitor() domain.SleepInhibitor {
	dll := windows.NewLazySystemDLL("kernel32.dll")
	return &windowsInhibitor{
		kernel32:           dll,
		setThreadExecState: dll.NewProc("SetThreadExecutionState"),
	}
}

func (i *windowsInhibitor) Inhibit() error {
	// The Win32 call reports the previous state; nothing here acts on it.
	_, _, _ = i.setThreadExecState.Call(uintptr(esContinuous | esSystemRequired))
	return nil
}

func (i *windowsInhibitor) Release() error {
	_, _, _ = i.setThreadExecState.Call(uintptr(esContinuous))
	return nil
}
