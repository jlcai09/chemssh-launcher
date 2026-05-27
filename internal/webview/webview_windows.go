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

	w := webview2.New(opts.Debug)
	if w == nil {
		return errors.New("could not initialize WebView2")
	}
	defer w.Destroy()
	configureChromium(w, opts)

	title := opts.Title
	if title == "" {
		title = "Chemweb Launcher"
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
	w.Run()
	close(done)
	return nil
}

type webViewSettingsProvider interface {
	GetSettings() (*edge.ICoreWebViewSettings, error)
}

func configureChromium(w webview2.WebView, opts Options) {
	provider, ok := chromiumSettingsProvider(w)
	if !ok {
		return
	}
	settings, err := provider.GetSettings()
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

func chromiumSettingsProvider(w webview2.WebView) (webViewSettingsProvider, bool) {
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
	provider, ok := browser.(webViewSettingsProvider)
	return provider, ok
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
      window.top.postMessage({ type: "chemweb-launcher:new-tab", url: absolute }, "*");
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
