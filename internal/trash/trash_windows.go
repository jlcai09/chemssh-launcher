//go:build windows

package trash

import (
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	foDelete          = 0x0003
	fofSilent         = 0x0004
	fofNoConfirmation = 0x0010
	fofAllowUndo      = 0x0040
	fofNoErrorUI      = 0x0400
)

type shFileOpStructW struct {
	hwnd                  windows.Handle
	wFunc                 uint32
	pFrom                 *uint16
	pTo                   *uint16
	fFlags                uint16
	fAnyOperationsAborted int32
	hNameMappings         uintptr
	lpszProgressTitle     *uint16
}

var (
	shell32              = windows.NewLazySystemDLL("shell32.dll")
	procSHFileOperationW = shell32.NewProc("SHFileOperationW")
)

func Move(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	from, err := windows.UTF16FromString(absolute)
	if err != nil {
		return err
	}
	from = append(from, 0)
	op := shFileOpStructW{
		wFunc:  foDelete,
		pFrom:  &from[0],
		fFlags: fofAllowUndo | fofNoConfirmation | fofSilent | fofNoErrorUI,
	}
	ret, _, callErr := procSHFileOperationW.Call(uintptr(unsafe.Pointer(&op)))
	if ret != 0 {
		if callErr != nil && callErr != syscall.Errno(0) {
			return fmt.Errorf("move to recycle bin: %w", callErr)
		}
		return fmt.Errorf("move to recycle bin: error code %d", ret)
	}
	if op.fAnyOperationsAborted != 0 {
		return fmt.Errorf("move to recycle bin: operation aborted")
	}
	return nil
}
