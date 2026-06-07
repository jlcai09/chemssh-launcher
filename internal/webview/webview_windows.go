//go:build windows && webview2

package webview

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/pkg/edge"
)

var dpiOnce sync.Once

const desktopChromeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

const (
	gwlpWndProc   = ^uintptr(3) // GWLP_WNDPROC = -4
	wmClose       = 0x0010
	mbYesNo       = 0x00000004
	mbIconWarning = 0x00000030
	idYes         = 6
	idNo          = 7
)

func Available() bool {
	return true
}

func DefaultEnabled() bool {
	return true
}

func Open(ctx context.Context, opts Options) error {
	if opts.URL == "" {
		return errors.New("webview url is required")
	}
	dpiOnce.Do(enableDPIAwareness)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:    opts.Debug,
		DataPath: opts.DataPath,
	})
	if w == nil {
		return errors.New("could not initialize WebView2")
	}
	defer w.Destroy()
	configureChromium(w, opts)
	if err := bindDownloadDialog(w); err != nil {
		log.Printf("warning: could not bind WebView2 downloads dialog: %v", err)
	}

	title := opts.Title
	if title == "" {
		title = "ChemSSH Launcher"
	}
	width := opts.Width
	if width <= 0 {
		width = 1180
	}
	height := opts.Height
	if height <= 0 {
		height = 780
	}

	w.SetTitle(title)
	w.SetSize(width, height, webview2.HintNone)
	setWindowIcon(w.Window())
	if opts.Fullscreen {
		setLargeRestoreBounds(w.Window())
		maximizeWindow(w.Window())
	}
	if opts.CopyOnCtrlShiftC || opts.DisableDevTools {
		w.Init(acceleratorScript(opts))
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			w.Dispatch(func() {
				w.Terminate()
			})
		case <-done:
		}
	}()

	w.Navigate(opts.URL)
	if opts.CloseInterceptor != nil {
		installCloseInterceptor(w, opts.CloseInterceptor)
	}
	w.Run()
	close(done)
	return nil
}

type webViewSettingsProvider interface {
	GetSettings() (*edge.ICoreWebViewSettings, error)
}

func configureChromium(w webview2.WebView, opts Options) {
	chromium, ok := chromiumFromWebView(w)
	if !ok {
		return
	}
	settings, err := chromium.GetSettings()
	if err != nil {
		log.Printf("warning: could not get WebView2 settings: %v", err)
		return
	}
	if err := settings.PutAreDefaultContextMenusEnabled(true); err != nil {
		log.Printf("warning: could not enable WebView2 context menus: %v", err)
	}
	if err := settings.PutAreDevToolsEnabled(!opts.DisableDevTools); err != nil {
		log.Printf("warning: could not configure WebView2 DevTools: %v", err)
	}
	userAgent := opts.UserAgent
	if userAgent == "" {
		userAgent = desktopChromeUserAgent
	}
	if err := settings.PutUserAgent(userAgent); err != nil {
		log.Printf("warning: could not configure WebView2 user agent: %v", err)
	}
}

func chromiumFromWebView(w webview2.WebView) (*edge.Chromium, bool) {
	value := reflect.ValueOf(w)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return nil, false
	}
	elem := value.Elem()
	if elem.Kind() != reflect.Struct {
		return nil, false
	}
	field := elem.FieldByName("browser")
	if !field.IsValid() || !field.CanAddr() {
		return nil, false
	}
	browser := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface()
	chromium, ok := browser.(*edge.Chromium)
	return chromium, ok
}

func bindDownloadDialog(w webview2.WebView) error {
	if err := w.Bind("chemsshOpenDownloads", func() error {
		return openDefaultDownloadDialog(w)
	}); err != nil {
		return err
	}
	return w.Bind("chemsshCloseDownloads", func() error {
		return closeDefaultDownloadDialog(w)
	})
}

type rawIUnknownVtbl struct {
	QueryInterface edge.ComProc
	AddRef         edge.ComProc
	Release        edge.ComProc
}

type rawCoreWebView2 struct {
	vtbl *rawIUnknownVtbl
}

type rawCoreWebView2_9 struct {
	vtbl *[97]edge.ComProc
}

func openDefaultDownloadDialog(w webview2.WebView) error {
	return callDefaultDownloadDialog(w, 91, "open WebView2 downloads dialog")
}

func closeDefaultDownloadDialog(w webview2.WebView) error {
	return callDefaultDownloadDialog(w, 92, "close WebView2 downloads dialog")
}

func callDefaultDownloadDialog(w webview2.WebView, methodIndex int, action string) error {
	chromium, ok := chromiumFromWebView(w)
	if !ok {
		return errors.New("WebView2 browser is not available")
	}
	value := reflect.ValueOf(chromium)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return errors.New("WebView2 browser is not initialized")
	}
	field := value.Elem().FieldByName("webview")
	if !field.IsValid() || field.IsNil() {
		return errors.New("WebView2 core is not available")
	}
	core := (*rawCoreWebView2)(unsafe.Pointer(field.Pointer()))
	iid := edge.NewGUID("{4D7B2EAB-9FDC-468D-B998-A9260B5ED651}")
	if iid == nil {
		return errors.New("invalid ICoreWebView2_9 GUID")
	}
	var dialog *rawCoreWebView2_9
	hr, _, _ := core.vtbl.QueryInterface.Call(
		uintptr(unsafe.Pointer(core)),
		uintptr(unsafe.Pointer(iid)),
		uintptr(unsafe.Pointer(&dialog)),
	)
	if int32(hr) < 0 || dialog == nil {
		return fmt.Errorf("WebView2 downloads dialog is not available: HRESULT 0x%08X", uint32(hr))
	}
	defer dialog.vtbl[2].Call(uintptr(unsafe.Pointer(dialog)))
	hr, _, _ = dialog.vtbl[methodIndex].Call(uintptr(unsafe.Pointer(dialog)))
	if int32(hr) < 0 {
		return fmt.Errorf("%s: HRESULT 0x%08X", action, uint32(hr))
	}
	return nil
}

func acceleratorScript(opts Options) string {
	disableDevTools := "true"
	if !opts.DisableDevTools {
		disableDevTools = "false"
	}
	copyOnCtrlShiftC := "true"
	if !opts.CopyOnCtrlShiftC {
		copyOnCtrlShiftC = "false"
	}
	return fmt.Sprintf(`(() => {
  const disableDevTools = %s;
  const copyOnCtrlShiftC = %s;
  async function copySelection() {
    const selection = String(window.getSelection ? window.getSelection() : "");
    if (selection && navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(selection);
      return;
    }
    document.execCommand("copy");
  }
  window.addEventListener("keydown", (event) => {
    const key = String(event.key || "").toLowerCase();
    const code = String(event.code || "").toLowerCase();
    const isC = key === "c" || code === "keyc";
    const isI = key === "i" || code === "keyi";
    const isJ = key === "j" || code === "keyj";
    const isDevToolsCombo = event.key === "F12" || (event.ctrlKey && event.shiftKey && (isC || isI || isJ));
    if (disableDevTools && isDevToolsCombo) {
      event.preventDefault();
      event.stopImmediatePropagation();
      if (copyOnCtrlShiftC && event.ctrlKey && event.shiftKey && isC) {
        copySelection().catch(() => {
          try { document.execCommand("copy"); } catch (_) {}
        });
      }
    }
  }, true);
  function requestShellTab(url) {
    if (!url) return false;
    try {
      const absolute = new URL(String(url), window.location.href).href;
      window.top.postMessage({ type: "chemssh-launcher:new-tab", url: absolute }, "*");
      return true;
    } catch (_) {
      return false;
    }
  }
  const originalOpen = window.open;
  window.open = function(url, target, features) {
    if (url && requestShellTab(url)) {
      return null;
    }
    return originalOpen.call(window, url, target, features);
  };
  window.addEventListener("click", (event) => {
    if (event.defaultPrevented || event.button !== 0) return;
    const anchor = event.target && event.target.closest ? event.target.closest("a[href]") : null;
    if (!anchor) return;
    const target = String(anchor.target || "").toLowerCase();
    const shouldOpenTab = target === "_blank" || event.ctrlKey || event.metaKey || event.shiftKey;
    if (!shouldOpenTab) return;
    if (requestShellTab(anchor.href)) {
      event.preventDefault();
      event.stopImmediatePropagation();
    }
  }, true);
  window.addEventListener("pointerdown", (event) => {
    const target = event.target;
    if (target && target.closest && target.closest("#downloads")) return;
    if (typeof window.chemsshCloseDownloads === "function") {
      window.chemsshCloseDownloads().catch(() => {});
      return;
    }
    try {
      window.top.postMessage({ type: "chemssh-launcher:close-downloads" }, "*");
    } catch (_) {}
  }, true);
})();`, disableDevTools, copyOnCtrlShiftC)
}

func enableDPIAwareness() {
	user32 := syscall.NewLazyDLL("user32.dll")
	setProcessDpiAwarenessContext := user32.NewProc("SetProcessDpiAwarenessContext")
	if err := setProcessDpiAwarenessContext.Find(); err == nil {
		const dpiAwarenessContextPerMonitorAwareV2 = ^uintptr(3)
		ret, _, _ := setProcessDpiAwarenessContext.Call(dpiAwarenessContextPerMonitorAwareV2)
		if ret != 0 {
			return
		}
	}

	shcore := syscall.NewLazyDLL("shcore.dll")
	setProcessDpiAwareness := shcore.NewProc("SetProcessDpiAwareness")
	if err := setProcessDpiAwareness.Find(); err == nil {
		const processPerMonitorDPIAware = uintptr(2)
		ret, _, _ := setProcessDpiAwareness.Call(processPerMonitorDPIAware)
		if ret == 0 {
			return
		}
	}

	setProcessDPIAware := user32.NewProc("SetProcessDPIAware")
	if err := setProcessDPIAware.Find(); err == nil {
		setProcessDPIAware.Call()
	}
}

func maximizeWindow(hwndPointer unsafe.Pointer) {
	if hwndPointer == nil {
		return
	}
	showWindow := syscall.NewLazyDLL("user32.dll").NewProc("ShowWindow")
	if err := showWindow.Find(); err != nil {
		return
	}
	const swMaximize = uintptr(3)
	showWindow.Call(uintptr(hwndPointer), swMaximize)
}

func setLargeRestoreBounds(hwndPointer unsafe.Pointer) {
	if hwndPointer == nil {
		return
	}
	user32 := syscall.NewLazyDLL("user32.dll")
	getSystemMetrics := user32.NewProc("GetSystemMetrics")
	setWindowPos := user32.NewProc("SetWindowPos")
	if err := getSystemMetrics.Find(); err != nil {
		return
	}
	if err := setWindowPos.Find(); err != nil {
		return
	}
	const (
		smCXScreen    = uintptr(0)
		smCYScreen    = uintptr(1)
		swpNoZOrder   = uintptr(0x0004)
		swpNoActivate = uintptr(0x0010)
	)
	screenW, _, _ := getSystemMetrics.Call(smCXScreen)
	screenH, _, _ := getSystemMetrics.Call(smCYScreen)
	if screenW == 0 || screenH == 0 {
		return
	}
	width := int(screenW * 86 / 100)
	height := int(screenH * 86 / 100)
	if width < 1280 {
		width = 1280
	}
	if height < 820 {
		height = 820
	}
	if width > int(screenW) {
		width = int(screenW)
	}
	if height > int(screenH) {
		height = int(screenH)
	}
	x := (int(screenW) - width) / 2
	y := (int(screenH) - height) / 2
	setWindowPos.Call(uintptr(hwndPointer), 0, uintptr(x), uintptr(y), uintptr(width), uintptr(height), swpNoZOrder|swpNoActivate)
}

func setWindowIcon(hwndPointer unsafe.Pointer) {
	if hwndPointer == nil {
		return
	}
	user32 := syscall.NewLazyDLL("user32.dll")
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getModuleHandle := kernel32.NewProc("GetModuleHandleW")
	loadImage := user32.NewProc("LoadImageW")
	sendMessage := user32.NewProc("SendMessageW")
	if err := getModuleHandle.Find(); err != nil {
		return
	}
	if err := loadImage.Find(); err != nil {
		return
	}
	if err := sendMessage.Find(); err != nil {
		return
	}
	name, err := syscall.UTF16PtrFromString("APP")
	if err != nil {
		return
	}
	module, _, _ := getModuleHandle.Call(0)
	if module == 0 {
		return
	}
	const (
		imageIcon     = uintptr(1)
		lrDefaultSize = uintptr(0x00000040)
		lrShared      = uintptr(0x00008000)
	)
	icon, _, _ := loadImage.Call(module, uintptr(unsafe.Pointer(name)), imageIcon, 0, 0, lrDefaultSize|lrShared)
	if icon == 0 {
		return
	}
	const (
		wmSetIcon  = uintptr(0x0080)
		iconSmall  = uintptr(0)
		iconBig    = uintptr(1)
		iconSmall2 = uintptr(2)
	)
	hwnd := uintptr(hwndPointer)
	sendMessage.Call(hwnd, wmSetIcon, iconBig, icon)
	sendMessage.Call(hwnd, wmSetIcon, iconSmall, icon)
	sendMessage.Call(hwnd, wmSetIcon, iconSmall2, icon)
}

// installCloseInterceptor subclasses the webview's native window to intercept
// WM_CLOSE. When the interceptor returns true (transfers in progress), a native
// MessageBox is shown; if the user clicks "No" the close is cancelled.
func installCloseInterceptor(w webview2.WebView, interceptor func() bool) {
	hwndPtr := w.Window()
	if hwndPtr == nil {
		return
	}
	hwnd := uintptr(hwndPtr)

	user32 := syscall.NewLazyDLL("user32.dll")
	getPtr := user32.NewProc("GetWindowLongPtrW")
	setPtr := user32.NewProc("SetWindowLongPtrW")
	callWndProc := user32.NewProc("CallWindowProcW")
	msgBox := user32.NewProc("MessageBoxW")

	if err := getPtr.Find(); err != nil {
		log.Printf("warning: GetWindowLongPtrW not available: %v", err)
		return
	}
	if err := setPtr.Find(); err != nil {
		log.Printf("warning: SetWindowLongPtrW not available: %v", err)
		return
	}

	oldProc, _, _ := getPtr.Call(hwnd, uintptr(gwlpWndProc))
	if oldProc == 0 {
		log.Printf("warning: could not get original wndproc")
		return
	}

	title, _ := syscall.UTF16PtrFromString("ChemSSH Launcher")
	text, _ := syscall.UTF16PtrFromString("\u6709\u4f20\u8f93\u4efb\u52a1\u6b63\u5728\u8fdb\u884c\u4e2d\u3002\n\u5173\u95ed\u7a97\u53e3\u5c06\u4e2d\u65ad\u8fd9\u4e9b\u4efb\u52a1\u3002\n\n\u786e\u5b9a\u8981\u5173\u95ed\u5417\uff1f\n\nTransfer tasks are still running.\nClosing this window will interrupt them.\n\nAre you sure you want to close?")

	newProc := syscall.NewCallback(func(hwndArg, msg, wp, lp uintptr) uintptr {
		if msg == wmClose {
			if interceptor() {
				ret, _, _ := msgBox.Call(
					hwndArg,
					uintptr(unsafe.Pointer(text)),
					uintptr(unsafe.Pointer(title)),
					uintptr(mbYesNo|mbIconWarning),
				)
				if ret != idYes {
					return 0 // cancel close
				}
			}
			// fall through to original wndproc (which calls DestroyWindow)
		}
		r, _, _ := callWndProc.Call(oldProc, hwndArg, msg, wp, lp)
		return r
	})

	setPtr.Call(hwnd, uintptr(gwlpWndProc), newProc)
}
