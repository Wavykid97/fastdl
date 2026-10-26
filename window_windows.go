//go:build windows

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/webviewloader"
	"golang.org/x/sys/windows/registry"
)

// The window must live on the program's main thread.
func init() { runtime.LockOSThread() }

var (
	dwmapi                    = syscall.NewLazyDLL("dwmapi.dll")
	dwmSetWindowAttribute     = dwmapi.NewProc("DwmSetWindowAttribute")
	procShowWindow            = user32.NewProc("ShowWindow")
	procSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	procIsIconic              = user32.NewProc("IsIconic")
	procFlashWindowEx         = user32.NewProc("FlashWindowEx")
	procGetForegroundWindow   = user32.NewProc("GetForegroundWindow")
	procAllowSetForegroundWin = user32.NewProc("AllowSetForegroundWindow")
)

// nativeWin is set while Laneway's own window is open.
var nativeWin webview2.WebView

// runNativeWindow shows the app in Laneway's own window, so the taskbar
// shows Laneway's icon and name (not Edge's). It blocks until the window is
// closed and returns true, or returns false straight away if the WebView2
// component isn't available (then the caller falls back to Edge).
func runNativeWindow(url string) bool {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		local = os.TempDir()
	}
	// Is the WebView2 component installed? (It comes with Windows 10/11.)
	if v, err := webviewloader.GetInstalledVersion(); err != nil || v == "" {
		return false
	}
	// If starting the window ever crashed the app, stick with the Edge
	// window for this version instead of crashing again. A marker file is
	// written before trying and removed once the window is up.
	marker := filepath.Join(local, appName, "window-starting")
	if data, err := os.ReadFile(marker); err == nil && string(data) == version {
		return false
	}
	os.MkdirAll(filepath.Dir(marker), 0o755)
	os.WriteFile(marker, []byte(version), 0o644)

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  filepath.Join(local, appName, "WebView2"),
		WindowOptions: webview2.WindowOptions{
			Title:  appName,
			Width:  900,
			Height: 720,
			IconId: 1, // the icon built into the .exe
			Center: true,
		},
	})
	if w == nil {
		os.Remove(marker)
		return false
	}
	os.Remove(marker) // it worked
	nativeWin = w
	hwnd := uintptr(w.Window())
	if systemUsesDarkMode() {
		on := int32(1)
		const dwmwaUseImmersiveDarkMode = 20
		dwmSetWindowAttribute.Call(hwnd, dwmwaUseImmersiveDarkMode, uintptr(unsafe.Pointer(&on)), 4)
	}
	w.SetSize(560, 480, webview2.HintMin) // resizable, but not too small
	w.Bind("lanewayFlash", func() { flashTaskbar(hwnd) })
	w.Navigate(url)
	w.Run()
	nativeWin = nil
	return true
}

// focusNativeWindow brings the open window to the front (used when the
// user opens Laneway again while it's already running).
func focusNativeWindow() bool {
	w := nativeWin
	if w == nil {
		return false
	}
	w.Dispatch(func() {
		hwnd := uintptr(w.Window())
		const swRestore = 9
		if r, _, _ := procIsIconic.Call(hwnd); r != 0 {
			procShowWindow.Call(hwnd, swRestore)
		}
		procSetForegroundWindow.Call(hwnd)
	})
	return true
}

// allowFocusSteal lets the already-running copy bring its window forward.
// Windows only lets the app the user just launched do that, so the new copy
// grants permission before asking.
func allowFocusSteal() {
	const asfwAny = ^uintptr(0)
	procAllowSetForegroundWin.Call(asfwAny)
}

// flashTaskbar flashes Laneway's taskbar button until the window is
// focused, if it isn't in front already.
func flashTaskbar(hwnd uintptr) {
	if fg, _, _ := procGetForegroundWindow.Call(); fg == hwnd {
		return
	}
	type flashInfo struct {
		size    uint32
		hwnd    uintptr
		flags   uint32
		count   uint32
		timeout uint32
	}
	const flashTray, flashTimerNoFG = 0x2, 0xC
	fi := flashInfo{hwnd: hwnd, flags: flashTray | flashTimerNoFG}
	fi.size = uint32(unsafe.Sizeof(fi))
	procFlashWindowEx.Call(uintptr(unsafe.Pointer(&fi)))
}

func systemUsesDarkMode() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	return err == nil && v == 0
}
