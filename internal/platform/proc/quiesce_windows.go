//go:build windows

package proc

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobBasicAccountingInformation matches JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
// ActiveProcesses is a custody observation, not the number ever assigned.
type jobBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// StopAndWait attests only Jobs that prohibited breakaway before the suspended
// root was resumed. Legacy Jobs can be killed, but their emptiness cannot prove
// that escaped writers are gone. PID enumeration is never a substitute.
func (t *Tree) StopAndWait(ctx context.Context) error {
	if t.quiescent {
		return nil
	}
	if !t.strictCustody || t.closed || t.job == 0 {
		return fmt.Errorf("workspace process has no retained non-breakaway Job: %w", ErrQuiescenceUnobservable)
	}
	if err := windows.TerminateJobObject(t.job, 1); err != nil {
		return errors.Join(ErrQuiescenceUnobservable, fmt.Errorf("terminate workspace Job: %w", err))
	}
	// Accounting may retain terminated processes until their handles close.
	// The command owner must also join cmd.Wait while this operation runs.
	if t.root != 0 {
		if err := windows.CloseHandle(t.root); err != nil {
			return errors.Join(ErrQuiescenceUnobservable, err)
		}
		t.root = 0
	}
	err := waitForQuiescence(ctx, func() (bool, error) {
		var info jobBasicAccountingInformation
		var size uint32
		err := windows.QueryInformationJobObject(t.job, windows.JobObjectBasicAccountingInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), &size)
		if err != nil {
			return false, err
		}
		if size != uint32(unsafe.Sizeof(info)) {
			return false, fmt.Errorf("incomplete Job accounting observation")
		}
		return info.ActiveProcesses == 0, nil
	})
	if err != nil {
		return err
	}
	// Keep the exact Job handle alive until the zero observation, then release.
	runtime.SetFinalizer(t, nil)
	_ = windows.CloseHandle(t.job)
	t.job = 0
	t.closed, t.quiescent = true, true
	return nil
}
