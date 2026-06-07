//go:build windows

package fileicon

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	fileAttributeReadOnly  = 0x00000001
	fileAttributeDirectory = 0x00000010
	fileAttributeNormal    = 0x00000080

	shgfiIcon              = 0x000000100
	shgfiLargeIcon         = 0x000000000
	shgfiSmallIcon         = 0x000000001
	shgfiUseFileAttributes = 0x000000010

	biRGB        = 0
	dibRGBColors = 0
	diNormal     = 0x0003
)

var (
	shell32              = windows.NewLazySystemDLL("shell32.dll")
	user32               = windows.NewLazySystemDLL("user32.dll")
	gdi32                = windows.NewLazySystemDLL("gdi32.dll")
	procSHGetFileInfoW   = shell32.NewProc("SHGetFileInfoW")
	procDestroyIcon      = user32.NewProc("DestroyIcon")
	procGetIconInfo      = user32.NewProc("GetIconInfo")
	procGetDC            = user32.NewProc("GetDC")
	procReleaseDC        = user32.NewProc("ReleaseDC")
	procDrawIconEx       = user32.NewProc("DrawIconEx")
	procGetObjectW       = gdi32.NewProc("GetObjectW")
	procCreateDIBSection = gdi32.NewProc("CreateDIBSection")
	procCreateCompatible = gdi32.NewProc("CreateCompatibleDC")
	procSelectObject     = gdi32.NewProc("SelectObject")
	procDeleteObject     = gdi32.NewProc("DeleteObject")
	procDeleteDC         = gdi32.NewProc("DeleteDC")
)

type shFileInfo struct {
	hIcon         windows.Handle
	iIcon         int32
	dwAttributes  uint32
	szDisplayName [260]uint16
	szTypeName    [80]uint16
}

type iconInfo struct {
	fIcon    int32
	xHotspot uint32
	yHotspot uint32
	hbmMask  windows.Handle
	hbmColor windows.Handle
}

type bitmap struct {
	bmType       int32
	bmWidth      int32
	bmHeight     int32
	bmWidthBytes int32
	bmPlanes     uint16
	bmBitsPixel  uint16
	bmBits       uintptr
}

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

type bitmapInfo struct {
	bmiHeader bitmapInfoHeader
	bmiColors [1]uint32
}

func loadSystemIconPNG(name string, isDir bool, size int) ([]byte, error) {
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("prepare icon path: %w", err)
	}

	attributes := uint32(fileAttributeNormal)
	if isDir {
		attributes = fileAttributeDirectory | fileAttributeReadOnly
	}
	flags := uintptr(shgfiIcon | shgfiUseFileAttributes)
	if normalizeSize(size) <= 16 {
		flags |= shgfiSmallIcon
	} else {
		flags |= shgfiLargeIcon
	}

	var info shFileInfo
	ret, _, err := procSHGetFileInfoW.Call(
		uintptr(unsafe.Pointer(path)),
		uintptr(attributes),
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
		flags,
	)
	if ret == 0 || info.hIcon == 0 {
		return nil, fmt.Errorf("get shell icon: %w", err)
	}
	defer procDestroyIcon.Call(uintptr(info.hIcon))

	return hiconToPNG(info.hIcon)
}

func hiconToPNG(hIcon windows.Handle) ([]byte, error) {
	var ii iconInfo
	ret, _, err := procGetIconInfo.Call(uintptr(hIcon), uintptr(unsafe.Pointer(&ii)))
	if ret == 0 {
		return nil, fmt.Errorf("get icon info: %w", err)
	}
	defer deleteGDIObject(ii.hbmColor)
	defer deleteGDIObject(ii.hbmMask)

	width, height, err := iconDimensions(ii)
	if err != nil {
		return nil, err
	}

	screenDC, _, err := procGetDC.Call(0)
	if screenDC == 0 {
		return nil, fmt.Errorf("get screen dc: %w", err)
	}
	defer procReleaseDC.Call(0, screenDC)

	memDC, _, err := procCreateCompatible.Call(screenDC)
	if memDC == 0 {
		return nil, fmt.Errorf("create compatible dc: %w", err)
	}
	defer procDeleteDC.Call(memDC)

	var bits unsafe.Pointer
	bi := bitmapInfo{
		bmiHeader: bitmapInfoHeader{
			biSize:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
			biWidth:       int32(width),
			biHeight:      -int32(height),
			biPlanes:      1,
			biBitCount:    32,
			biCompression: biRGB,
		},
	}
	hBitmap, _, err := procCreateDIBSection.Call(
		screenDC,
		uintptr(unsafe.Pointer(&bi)),
		dibRGBColors,
		uintptr(unsafe.Pointer(&bits)),
		0,
		0,
	)
	if hBitmap == 0 || bits == nil {
		return nil, fmt.Errorf("create icon bitmap: %w", err)
	}
	defer procDeleteObject.Call(hBitmap)

	oldObject, _, _ := procSelectObject.Call(memDC, hBitmap)
	if oldObject != 0 {
		defer procSelectObject.Call(memDC, oldObject)
	}

	ret, _, err = procDrawIconEx.Call(memDC, 0, 0, uintptr(hIcon), uintptr(width), uintptr(height), 0, 0, diNormal)
	if ret == 0 {
		return nil, fmt.Errorf("draw icon: %w", err)
	}

	raw := unsafe.Slice((*byte)(bits), width*height*4)
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	anyAlpha := false
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			src := (y*width + x) * 4
			dst := y*img.Stride + x*4
			b := raw[src]
			g := raw[src+1]
			r := raw[src+2]
			a := raw[src+3]
			if a != 0 {
				anyAlpha = true
			}
			img.Pix[dst] = r
			img.Pix[dst+1] = g
			img.Pix[dst+2] = b
			img.Pix[dst+3] = a
		}
	}
	// If the icon has no alpha channel (24-bit icon), DrawIconEx has already
	// composed the AND/XOR masks onto our 32-bit DIB. All pixels that survived
	// the composition should be treated as fully opaque.
	if !anyAlpha {
		for i := 0; i < len(img.Pix); i += 4 {
			img.Pix[i+3] = 255
		}
	}

	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, fmt.Errorf("encode icon png: %w", err)
	}
	return out.Bytes(), nil
}

func iconDimensions(ii iconInfo) (int, int, error) {
	handle := ii.hbmColor
	monochrome := false
	if handle == 0 {
		handle = ii.hbmMask
		monochrome = true
	}
	if handle == 0 {
		return 0, 0, fmt.Errorf("icon has no bitmap")
	}
	var bm bitmap
	ret, _, err := procGetObjectW.Call(uintptr(handle), unsafe.Sizeof(bm), uintptr(unsafe.Pointer(&bm)))
	if ret == 0 {
		return 0, 0, fmt.Errorf("get icon bitmap object: %w", err)
	}
	width := int(bm.bmWidth)
	height := int(bm.bmHeight)
	if monochrome {
		height /= 2
	}
	if width <= 0 || height <= 0 {
		return 0, 0, fmt.Errorf("invalid icon bitmap size")
	}
	return width, height, nil
}

func deleteGDIObject(handle windows.Handle) {
	if handle != 0 {
		procDeleteObject.Call(uintptr(handle))
	}
}
