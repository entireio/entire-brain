package parentwatch

import "golang.org/x/sys/windows"

// watchParent opens a SYNCHRONIZE handle on the parent and returns a wait
// that blocks in WaitForSingleObject until the parent exits. A failed open
// (including a recycled ppid) surfaces as an error and Arm degrades to no
// watchdog — today's behavior. The wait returns only on a signaled handle;
// any other outcome blocks forever rather than killing a healthy process.
func watchParent(ppid int) (func(), error) {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(ppid))
	if err != nil {
		return nil, err
	}
	return func() {
		status, err := windows.WaitForSingleObject(handle, windows.INFINITE)
		if err == nil && status == windows.WAIT_OBJECT_0 {
			return
		}
		select {}
	}, nil
}
