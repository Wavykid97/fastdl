//go:build windows

package main

import (
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	messageBoxW             = user32.NewProc("MessageBoxW")
	attachConsole           = kernel32.NewProc("AttachConsole")
	setThreadExecutionState = kernel32.NewProc("SetThreadExecutionState")
)

var (
	awakeOnce sync.Once
	awakeCh   = make(chan bool, 4)
)

// keepAwake stops Windows from going to sleep while downloads are running
// (the screen can still turn off). Windows ties this setting to one thread,
// so a dedicated thread owns it.
func keepAwake(on bool) {
	awakeOnce.Do(func() {
		go func() {
			runtime.LockOSThread()
			const esContinuous, esSystemRequired = 0x80000000, 0x00000001
			for on := range awakeCh {
				flags := uintptr(esContinuous)
				if on {
					flags |= esSystemRequired
				}
				setThreadExecutionState.Call(flags)
			}
		}()
	})
	awakeCh <- on
}

// fatalBox shows a normal Windows error dialog (there's no console to print to).
func fatalBox(msg string) {
	title, _ := syscall.UTF16PtrFromString(appName)
	text, _ := syscall.UTF16PtrFromString(msg)
	const mbIconError = 0x10
	messageBoxW.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), mbIconError)
}

// useParentConsole lets command-line mode print into the Command Prompt or
// PowerShell window it was started from, even though the app is built
// without a console of its own.
func useParentConsole() {
	const attachParentProcess = ^uintptr(0) // (DWORD)-1
	if r, _, _ := attachConsole.Call(attachParentProcess); r == 0 {
		return
	}
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout = f
		os.Stderr = f
	}
	if f, err := os.OpenFile("CONIN$", os.O_RDONLY, 0); err == nil {
		os.Stdin = f
	}
	os.Stdout.WriteString("\n")
}

// startDetached starts Laneway so that it keeps running after the browser
// closes the extension's helper process. Browsers may put that helper in a
// job that ends its children, so ask to break away; if that's not allowed,
// let Explorer start it instead.
func startDetached(exe string) error {
	const detachedProcess, newGroup, breakaway = 0x00000008, 0x00000200, 0x01000000
	cmd := exec.Command(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | newGroup | breakaway}
	if err := cmd.Start(); err == nil {
		cmd.Process.Release()
		return nil
	}
	return exec.Command("explorer.exe", exe).Start()
}

// diskFree is the free space on the drive holding dir (0 if unknown).
func diskFree(dir string) uint64 {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0
	}
	var free uint64
	if windows.GetDiskFreeSpaceEx(p, &free, nil, nil) != nil {
		return 0
	}
	return free
}

// hideFile hides the folder being extracted into, so it doesn't look like
// a finished folder while files are still arriving.
func hideFile(p string) {
	if u, err := windows.UTF16PtrFromString(p); err == nil {
		if a, err := windows.GetFileAttributes(u); err == nil {
			windows.SetFileAttributes(u, a|windows.FILE_ATTRIBUTE_HIDDEN)
		}
	}
}

func unhideFile(p string) {
	if u, err := windows.UTF16PtrFromString(p); err == nil {
		if a, err := windows.GetFileAttributes(u); err == nil {
			windows.SetFileAttributes(u, a&^windows.FILE_ATTRIBUTE_HIDDEN)
		}
	}
}
