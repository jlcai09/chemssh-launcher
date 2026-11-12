//go:build windows && webview2

package webview

import (
	"log"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

const webviewStartupTimeout = 10 * time.Second

const webviewStartupTimeoutMessage = `ChemSSH Launcher 启动超时：WebView2 页面在 10 秒内没有完成加载。

启动器已尝试自动清理残留的 WebView2 进程。如果问题仍然存在，请关闭启动器后执行下面的 PowerShell 命令，或重启电脑：

Get-CimInstance Win32_Process -Filter "Name='msedgewebview2.exe'" | Where-Object CommandLine -like '*ChemSSHLauncher*' | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }

Startup timed out: the WebView2 page did not finish loading within 10 seconds.`

// watchWebViewStartup exits the process with an actionable dialog when the
// window never gets a frontend ready signal. It runs from the moment Open is
// called so it also covers a hang inside WebView2 environment creation, where
// no window message loop exists yet.
func watchWebViewStartup(ready <-chan struct{}, stop <-chan struct{}) {
	timer := time.NewTimer(webviewStartupTimeout)
	defer timer.Stop()
	select {
	case <-ready:
		return
	case <-stop:
		return
	case <-timer.C:
	}
	select {
	case <-ready:
		return
	case <-stop:
		return
	default:
	}
	log.Printf("error: WebView2 did not finish loading within %s; exiting", webviewStartupTimeout)
	showStartupTimeoutDialog()
	os.Exit(1)
}

func showStartupTimeoutDialog() {
	text, err := windows.UTF16PtrFromString(webviewStartupTimeoutMessage)
	if err != nil {
		return
	}
	title, err := windows.UTF16PtrFromString("ChemSSH Launcher")
	if err != nil {
		return
	}
	_, _ = windows.MessageBox(0, text, title, windows.MB_OK|windows.MB_ICONERROR|windows.MB_TOPMOST|windows.MB_SETFOREGROUND)
}
