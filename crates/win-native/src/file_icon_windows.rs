#![cfg(target_os = "windows")]

use std::mem::size_of;

use windows::core::PCWSTR;
use windows::Win32::Graphics::Gdi::{
    CreateCompatibleDC, DeleteDC, DeleteObject, GetDIBits, GetObjectW, SelectObject, BITMAP,
    BITMAPINFO, BITMAPINFOHEADER, BI_RGB, DIB_RGB_COLORS, HBITMAP,
};
use windows::Win32::Storage::FileSystem::{FILE_ATTRIBUTE_DIRECTORY, FILE_ATTRIBUTE_NORMAL};
use windows::Win32::UI::Shell::{
    SHGetFileInfoW, SHFILEINFOW, SHGFI_ICON, SHGFI_LARGEICON, SHGFI_SMALLICON,
    SHGFI_USEFILEATTRIBUTES,
};
use windows::Win32::UI::WindowsAndMessaging::{DestroyIcon, GetIconInfo, HICON, ICONINFO};

use crate::file_icon::encode_rgba_png;

pub fn extract_windows_icon_png(
    lookup_name: &str,
    is_dir: bool,
    size: u32,
) -> Result<Vec<u8>, String> {
    let wide: Vec<u16> = lookup_name.encode_utf16().chain(Some(0)).collect();
    let attrs = if is_dir {
        FILE_ATTRIBUTE_DIRECTORY.0
    } else {
        FILE_ATTRIBUTE_NORMAL.0
    };
    let size_flag = if size <= 16 {
        SHGFI_SMALLICON
    } else {
        SHGFI_LARGEICON
    };

    let mut info = SHFILEINFOW::default();
    let res = unsafe {
        SHGetFileInfoW(
            PCWSTR(wide.as_ptr()),
            windows::Win32::Storage::FileSystem::FILE_FLAGS_AND_ATTRIBUTES(attrs),
            Some(&mut info),
            size_of::<SHFILEINFOW>() as u32,
            SHGFI_ICON | SHGFI_USEFILEATTRIBUTES | size_flag,
        )
    };
    if res == 0 || info.hIcon.is_invalid() {
        return Err("SHGetFileInfoW failed to resolve shell icon".into());
    }

    let _icon_guard = IconGuard(info.hIcon);
    hicon_to_png(info.hIcon)
}

struct IconGuard(HICON);
impl Drop for IconGuard {
    fn drop(&mut self) {
        if !self.0.is_invalid() {
            unsafe {
                let _ = DestroyIcon(self.0);
            }
        }
    }
}

struct BitmapGuard(HBITMAP);
impl Drop for BitmapGuard {
    fn drop(&mut self) {
        if !self.0.is_invalid() {
            unsafe {
                let _ = DeleteObject(self.0);
            }
        }
    }
}

fn hicon_to_png(hicon: HICON) -> Result<Vec<u8>, String> {
    let mut icon_info = ICONINFO::default();
    unsafe { GetIconInfo(hicon, &mut icon_info) }
        .map_err(|e| format!("GetIconInfo failed: {e}"))?;
    let _color_guard = BitmapGuard(icon_info.hbmColor);
    let _mask_guard = BitmapGuard(icon_info.hbmMask);

    let target_bmp = if !icon_info.hbmColor.is_invalid() {
        icon_info.hbmColor
    } else {
        icon_info.hbmMask
    };
    if target_bmp.is_invalid() {
        return Err("icon has no color or mask bitmap".into());
    }

    let mut bmp = BITMAP::default();
    let obj_bytes = unsafe {
        GetObjectW(
            target_bmp,
            size_of::<BITMAP>() as i32,
            Some(&mut bmp as *mut BITMAP as *mut core::ffi::c_void),
        )
    };
    if obj_bytes == 0 {
        return Err("GetObjectW failed on icon bitmap".into());
    }

    let width = bmp.bmWidth.max(1) as u32;
    let raw_height = if !icon_info.hbmColor.is_invalid() {
        bmp.bmHeight.abs().max(1)
    } else {
        (bmp.bmHeight.abs() / 2).max(1)
    };
    let height = raw_height as u32;

    let hdc = unsafe { CreateCompatibleDC(None) };
    if hdc.is_invalid() {
        return Err("CreateCompatibleDC failed".into());
    }

    let result = (|| {
        let pixel_bytes_len = (width as usize)
            .checked_mul(height as usize)
            .and_then(|n| n.checked_mul(4))
            .ok_or_else(|| "icon bitmap dimensions overflow".to_string())?;
        let mut bgra = vec![0u8; pixel_bytes_len];

        let mut bmi = BITMAPINFO::default();
        bmi.bmiHeader = BITMAPINFOHEADER {
            biSize: size_of::<BITMAPINFOHEADER>() as u32,
            biWidth: width as i32,
            biHeight: -(height as i32), // top-down DIB
            biPlanes: 1,
            biBitCount: 32,
            biCompression: BI_RGB.0,
            ..Default::default()
        };

        let old_obj = unsafe { SelectObject(hdc, target_bmp) };
        let scanlines = unsafe {
            GetDIBits(
                hdc,
                target_bmp,
                0,
                height,
                Some(bgra.as_mut_ptr() as *mut core::ffi::c_void),
                &mut bmi,
                DIB_RGB_COLORS,
            )
        };
        unsafe {
            SelectObject(hdc, old_obj);
        }
        if scanlines == 0 {
            return Err("GetDIBits returned zero scanlines".into());
        }

        // Convert BGRA -> RGBA and handle icons without an alpha channel via the mask bitmap.
        let has_nonzero_alpha = bgra.chunks_exact(4).any(|px| px[3] != 0);
        let mask_alpha = if !has_nonzero_alpha && !icon_info.hbmMask.is_invalid() {
            read_mask_alpha(hdc, icon_info.hbmMask, width, height)
        } else {
            None
        };

        let mut rgba = vec![0u8; pixel_bytes_len];
        for (i, px) in bgra.chunks_exact(4).enumerate() {
            let dst = i * 4;
            rgba[dst] = px[2];
            rgba[dst + 1] = px[1];
            rgba[dst + 2] = px[0];
            rgba[dst + 3] = if has_nonzero_alpha {
                px[3]
            } else if let Some(ref mask) = mask_alpha {
                mask[i]
            } else {
                255
            };
        }

        encode_rgba_png(width, height, &rgba)
    })();

    unsafe {
        let _ = DeleteDC(hdc);
    }
    result
}

fn read_mask_alpha(
    hdc: windows::Win32::Graphics::Gdi::HDC,
    mask_bmp: HBITMAP,
    width: u32,
    height: u32,
) -> Option<Vec<u8>> {
    let pixel_count = (width as usize).checked_mul(height as usize)?;
    let mut mask_bgra = vec![0u8; pixel_count * 4];
    let mut bmi = BITMAPINFO::default();
    bmi.bmiHeader = BITMAPINFOHEADER {
        biSize: size_of::<BITMAPINFOHEADER>() as u32,
        biWidth: width as i32,
        biHeight: -(height as i32),
        biPlanes: 1,
        biBitCount: 32,
        biCompression: BI_RGB.0,
        ..Default::default()
    };
    let old_obj = unsafe { SelectObject(hdc, mask_bmp) };
    let scanlines = unsafe {
        GetDIBits(
            hdc,
            mask_bmp,
            0,
            height,
            Some(mask_bgra.as_mut_ptr() as *mut core::ffi::c_void),
            &mut bmi,
            DIB_RGB_COLORS,
        )
    };
    unsafe {
        SelectObject(hdc, old_obj);
    }
    if scanlines == 0 {
        return None;
    }
    // AND mask: white (0xff) means transparent, black (0x00) means opaque.
    Some(
        mask_bgra
            .chunks_exact(4)
            .map(|px| if px[0] > 127 { 0 } else { 255 })
            .collect(),
    )
}
