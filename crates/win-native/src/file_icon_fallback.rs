use crate::file_icon::encode_rgba_png;

pub fn render_fallback_icon_png(is_dir: bool, ext: &str, size: u32) -> Vec<u8> {
    let s = if size <= 16 { 16usize } else { 32usize };
    let mut rgba = vec![0u8; s * s * 4];

    if is_dir {
        draw_folder_icon(&mut rgba, s);
    } else {
        let accent = extension_accent(ext);
        draw_file_icon(&mut rgba, s, accent);
    }

    encode_rgba_png(s as u32, s as u32, &rgba).unwrap_or_default()
}

fn extension_accent(ext: &str) -> [u8; 4] {
    match ext {
        ".py" | ".ipynb" => [43, 125, 233, 255],
        ".sh" | ".bash" | ".zsh" | ".ps1" => [34, 154, 87, 255],
        ".json" | ".yaml" | ".yml" | ".toml" | ".xml" => [217, 119, 6, 255],
        ".md" | ".txt" | ".log" | ".csv" => [100, 116, 139, 255],
        ".zip" | ".tar" | ".gz" | ".7z" | ".rar" => [147, 51, 234, 255],
        ".png" | ".jpg" | ".jpeg" | ".gif" | ".svg" | ".webp" => [14, 165, 233, 255],
        ".pdf" => [220, 38, 38, 255],
        _ => [79, 128, 224, 255],
    }
}

fn draw_folder_icon(buf: &mut [u8], size: usize) {
    let scale = size / 16;
    let body_top = 4 * scale;
    let body_bottom = 14 * scale;
    let left = scale;
    let right = 15 * scale;
    let tab_top = 2 * scale;
    let tab_right = 7 * scale;

    for y in tab_top..body_top {
        for x in left..tab_right {
            set_px(buf, size, x, y, [217, 160, 54, 255]);
        }
    }
    for y in body_top..body_bottom {
        for x in left..right {
            let color = if y == body_top || y + 1 == body_bottom || x == left || x + 1 == right {
                [214, 152, 42, 255]
            } else if y < body_top + 2 * scale {
                [252, 211, 77, 255]
            } else {
                [245, 190, 65, 255]
            };
            set_px(buf, size, x, y, color);
        }
    }
}

fn draw_file_icon(buf: &mut [u8], size: usize, accent: [u8; 4]) {
    let scale = size / 16;
    let left = 2 * scale;
    let right = 14 * scale;
    let top = scale;
    let bottom = 15 * scale;
    let fold = 4 * scale;

    for y in top..bottom {
        for x in left..right {
            let in_fold_corner = x >= right - fold && y < top + fold && (x - (right - fold)) > (y - top);
            if in_fold_corner {
                continue;
            }
            let border = x == left
                || x + 1 == right
                || y == top
                || y + 1 == bottom
                || (x >= right - fold && y < top + fold && (x - (right - fold)) == (y - top));
            let color = if border {
                [148, 163, 184, 255]
            } else if x >= right - fold && y < top + fold {
                [226, 232, 240, 255]
            } else {
                [248, 250, 252, 255]
            };
            set_px(buf, size, x, y, color);
        }
    }

    // Accent header bar inside file page
    for y in (top + 5 * scale)..(top + 7 * scale) {
        for x in (left + 2 * scale)..(right - 3 * scale) {
            set_px(buf, size, x, y, accent);
        }
    }
    for y in (top + 8 * scale)..(top + 10 * scale) {
        for x in (left + 2 * scale)..(right - 4 * scale) {
            set_px(buf, size, x, y, [203, 213, 225, 255]);
        }
    }
}

#[inline]
fn set_px(buf: &mut [u8], size: usize, x: usize, y: usize, color: [u8; 4]) {
    if x < size && y < size {
        let idx = (y * size + x) * 4;
        buf[idx..idx + 4].copy_from_slice(&color);
    }
}
