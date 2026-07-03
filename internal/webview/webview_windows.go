//go:build windows && webview2

package webview

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"reflect"
	"runtime"
	"sync"
	"syscall"
	"time"
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
	// Apply Chromium flags to reduce memory overhead from unnecessary
	// features while keeping GPU acceleration intact. Must run before
	// NewWithOptions because the WebView2 loader reads the env var at
	// environment creation time.
	applyMemoryOptimisationFlags()
	dpiOnce.Do(enableDPIAwareness)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

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

	// The go-webview2 library forcibly shows the native window the moment it is
	// created (webview.go CreateWithOptions: ShowWindow+UpdateWindow), before the
	// WebView2 runtime is embedded and before any navigation. That blank window
	// stays visible through Embed + Navigate + the Vue bundle load, which is the
	// startup flicker. We can't stop the initial show, but for fullscreen we
	// create a 1x1 window so that flash is effectively invisible, then keep the
	// window hidden until the page's first paint is ready.
	initialWidth, initialHeight := width, height
	if opts.Fullscreen {
		initialWidth, initialHeight = 1, 1
	}

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:    opts.Debug,
		DataPath: opts.DataPath,
		WindowOptions: webview2.WindowOptions{
			Title:  title,
			Width:  uint(initialWidth),
			Height: uint(initialHeight),
			IconId: 1,
			Center: true,
		},
	})
	if w == nil {
		return errors.New("could not initialize WebView2")
	}
	defer w.Destroy()

	// Hide the window immediately; it will be revealed once content is ready.
	hideWindow(w.Window())

	setWindowIcon(w.Window())
	configureChromium(w, opts)
	if err := bindDownloadDialog(w); err != nil {
		log.Printf("warning: could not bind WebView2 downloads dialog: %v", err)
	}

	w.SetTitle(title)

	// Size the hidden window to its final dimensions so the page lays out at the
	// correct size before it ever becomes visible (no resize reflow on reveal).
	if opts.Fullscreen {
		setRestoreBounds(w.Window(), width, height)
	} else {
		w.SetSize(width, height, webview2.HintNone)
	}

	// reveal shows the window once. It is triggered either by the frontend (as
	// soon as the loading page is parsed) or by the fallback timer below.
	var revealOnce sync.Once
	reveal := func() {
		revealOnce.Do(func() {
			if opts.Fullscreen {
				maximizeWindow(w.Window())
			} else {
				showWindowVisible(w.Window())
			}
		})
	}
	if err := w.Bind("__chemsshRevealWindow", func() {
		reveal()
	}); err != nil {
		log.Printf("warning: could not bind WebView2 reveal hook: %v", err)
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

	// Fallback reveal: if the frontend never calls the reveal hook (e.g. a script
	// error), make sure the window still appears instead of staying hidden.
	go func() {
		select {
		case <-time.After(2 * time.Second):
			w.Dispatch(reveal)
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

// applyMemoryOptimisationFlags sets WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS to
// disable Chromium features that are unnecessary for a dedicated launcher
// window. GPU hardware acceleration is preserved.
//
// The WebView2 loader reads this environment variable when
// CreateCoreWebView2EnvironmentWithOptions is called (inside NewWithOptions).
//
// Expected reduction: 100-250 MB (from disabling background services, extra
// processes, and SmartScreen telemetry).
func applyMemoryOptimisationFlags() {
	const envKey = "WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS"
	if os.Getenv(envKey) != "" {
		// Respect user-supplied flags; do not overwrite.
		return
	}
	// Each flag targets a specific memory-consuming feature:
	//
	// --disable-features=...
	//   SmartScreen               - URL reputation service (~30-50MB)
	//   msSmartScreenProtection   - Edge SmartScreen variant
	//   TranslateUI               - translation bubble (unused)
	//   msEdgeHub                 - Edge Hub sidebar (Edge-specific)
	//   ReadingList               - Edge reading list (Edge-specific)
	//   msCollections              - Edge collections feature
	//
	// --disable-background-networking
	//   Stops background telemetry, update pings, and OCSP checks.
	//
	// --disable-component-update
	//   Prevents Chromium component downloads (Widevine, etc.).
	//
	// --disable-extensions
	//   Prevents loading any Chrome extensions.
	//
	// --disable-sync
	//   Disables Chrome account sync (bookmarks, settings, etc.).
	//
	// --disable-client-side-phishing-detection
	//   Turns off the phishing detection model (~10-20MB).
	//
	// --no-first-run --no-default-browser-check
	//   Skip first-run dialogs and default-browser prompts.
	//
	// --metrics-recording-only
	//   Prevents UMA metrics upload (saves a background task).
	//
	// --disable-accelerated-video-decode
	//   Prevents GPU video decoder init (saves ~30-80MB GPU memory
	//   on pages with no video; has no effect on static UI pages).
	flags := "" +
		"--disable-features=SmartScreen,msSmartScreenProtection,TranslateUI,msEdgeHub,ReadingList,msCollections " +
		"--disable-background-networking " +
		"--disable-component-update " +
		"--disable-extensions " +
		"--disable-sync " +
		"--disable-client-side-phishing-detection " +
		"--no-first-run " +
		"--no-default-browser-check " +
		"--metrics-recording-only " +
		"--disable-accelerated-video-decode"
	os.Setenv(envKey, flags)
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

func hideWindow(hwndPointer unsafe.Pointer) {
	if hwndPointer == nil {
		return
	}
	showWindow := syscall.NewLazyDLL("user32.dll").NewProc("ShowWindow")
	if err := showWindow.Find(); err != nil {
		return
	}
	const swHide = uintptr(0)
	showWindow.Call(uintptr(hwndPointer), swHide)
}

func showWindowVisible(hwndPointer unsafe.Pointer) {
	if hwndPointer == nil {
		return
	}
	showWindow := syscall.NewLazyDLL("user32.dll").NewProc("ShowWindow")
	if err := showWindow.Find(); err != nil {
		return
	}
	const swShow = uintptr(5)
	showWindow.Call(uintptr(hwndPointer), swShow)
}

// setRestoreBounds gives a fullscreen-starting window a comfortable restored
// size before it is maximized. Windows uses these bounds when the user clicks
// the maximize button to restore the window.
func setRestoreBounds(hwndPointer unsafe.Pointer, preferredWidth, preferredHeight int) {
	if hwndPointer == nil {
		return
	}
	user32 := syscall.NewLazyDLL("user32.dll")
	systemParametersInfo := user32.NewProc("SystemParametersInfoW")
	setWindowPos := user32.NewProc("SetWindowPos")
	if err := systemParametersInfo.Find(); err != nil {
		return
	}
	if err := setWindowPos.Find(); err != nil {
		return
	}
	type rect struct {
		Left, Top, Right, Bottom int32
	}
	const spiGetWorkArea = uintptr(0x0030)
	var wa rect
	ret, _, _ := systemParametersInfo.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&wa)), 0)
	if ret == 0 {
		return
	}
	width := wa.Right - wa.Left
	height := wa.Bottom - wa.Top
	if width <= 0 || height <= 0 {
		return
	}
	restoreWidth := clampRestoreSize(preferredWidth, int(width))
	restoreHeight := clampRestoreSize(preferredHeight, int(height))
	x := int(wa.Left) + (int(width)-restoreWidth)/2
	y := int(wa.Top) + (int(height)-restoreHeight)/2
	const (
		swpNoZOrder   = uintptr(0x0004)
		swpNoActivate = uintptr(0x0010)
	)
	setWindowPos.Call(
		uintptr(hwndPointer), 0,
		uintptr(x), uintptr(y), uintptr(restoreWidth), uintptr(restoreHeight),
		swpNoZOrder|swpNoActivate,
	)
}

func clampRestoreSize(preferred, workAreaSize int) int {
	if workAreaSize <= 0 {
		return preferred
	}
	if preferred <= 0 {
		preferred = workAreaSize * 72 / 100
	}
	size := preferred
	if scaled := workAreaSize * 78 / 100; size < scaled {
		size = scaled
	}
	if max := workAreaSize * 88 / 100; size > max {
		size = max
	}
	if size < 1 {
		return 1
	}
	return size
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
	module, _, _ := getModuleHandle.Call(0)
	if module == 0 {
		return
	}
	const (
		imageIcon     = uintptr(1)
		lrDefaultSize = uintptr(0x00000040)
		lrShared      = uintptr(0x00008000)
	)
	icon, _, _ := loadImage.Call(module, 1, imageIcon, 0, 0, lrDefaultSize|lrShared)
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
