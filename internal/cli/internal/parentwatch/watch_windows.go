package parentwatch

import "golang.org/x/sys/windows"

// watchParent opens a SYNCHRONIZE handle on the parent and returns a wait
// that blocks in WaitForSingleObject until the parent exits. A failed open
// (including a recycled ppid) surfaces as an error and Arm degrades to no
// watchdog — today's behavior. The wait reports true only for a signaled
// handle, so a wait failure can never kill a healthy process.
func watchParent(ppid int) (func() bool, error) {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(ppid))
	if err != nil {
		return nil, err
	}
	return func() bool {
		status, err := windows.WaitForSingleObject(handle, windows.INFINITE)
		return err == nil && status == windows.WAIT_OBJECT_0
	}, nil
}
