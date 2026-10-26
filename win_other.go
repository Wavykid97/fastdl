//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func fatalBox(msg string) { fmt.Fprintln(os.Stderr, msg) }
func useParentConsole()   {}
func keepAwake(on bool)   {}

func runNativeWindow(url string) bool { return false }
func focusNativeWindow() bool         { return false }
func allowFocusSteal()                {}

func startDetached(exe string) error {
	cmd := exec.Command(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// diskFree is the free space on the drive holding dir (0 if unknown).
func diskFree(dir string) uint64 {
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil {
		return 0
	}
	return st.Bavail * uint64(st.Bsize)
}

func hideFile(string)   {}
func unhideFile(string) {}
