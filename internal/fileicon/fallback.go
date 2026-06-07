package fileicon

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
)

func fallbackIconPNG(isDir bool, size int) ([]byte, error) {
	size = normalizeSize(size)
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	if isDir {
		drawFolderIcon(img, size)
	} else {
		drawFileIcon(img, size)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func drawFileIcon(img *image.NRGBA, size int) {
	border := color.NRGBA{R: 112, G: 128, B: 144, A: 255}
	fill := color.NRGBA{R: 248, G: 250, B: 252, A: 255}
	fold := color.NRGBA{R: 224, G: 231, B: 239, A: 255}
	x0, y0 := size/4, size/8
	x1, y1 := size-size/5, size-size/8
	foldSize := max(4, size/4)
	fillRect(img, x0, y0, x1, y1, fill)
	fillRect(img, x1-foldSize, y0, x1, y0+foldSize, fold)
	drawRect(img, x0, y0, x1, y1, border)
	drawLine(img, x1-foldSize, y0, x1, y0+foldSize, border)
	drawLine(img, x1-foldSize, y0+foldSize, x1, y0+foldSize, border)
}

func drawFolderIcon(img *image.NRGBA, size int) {
	border := color.NRGBA{R: 168, G: 124, B: 32, A: 255}
	tab := color.NRGBA{R: 250, G: 204, B: 86, A: 255}
	body := color.NRGBA{R: 244, G: 180, B: 52, A: 255}
	x0, y0 := size/8, size/4
	x1, y1 := size-size/8, size-size/6
	tabRight := x0 + size/3
	fillRect(img, x0, y0, tabRight, y0+size/5, tab)
	fillRect(img, x0, y0+size/8, x1, y1, body)
	drawRect(img, x0, y0+size/8, x1, y1, border)
	drawLine(img, x0, y0, tabRight, y0, border)
	drawLine(img, tabRight, y0, tabRight+size/8, y0+size/8, border)
}

func fillRect(img *image.NRGBA, x0, y0, x1, y1 int, c color.NRGBA) {
	b := img.Bounds()
	x0 = clamp(x0, b.Min.X, b.Max.X)
	y0 = clamp(y0, b.Min.Y, b.Max.Y)
	x1 = clamp(x1, b.Min.X, b.Max.X)
	y1 = clamp(y1, b.Min.Y, b.Max.Y)
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
}

func drawRect(img *image.NRGBA, x0, y0, x1, y1 int, c color.NRGBA) {
	drawLine(img, x0, y0, x1-1, y0, c)
	drawLine(img, x0, y1-1, x1-1, y1-1, c)
	drawLine(img, x0, y0, x0, y1-1, c)
	drawLine(img, x1-1, y0, x1-1, y1-1, c)
}

func drawLine(img *image.NRGBA, x0, y0, x1, y1 int, c color.NRGBA) {
	dx := abs(x1 - x0)
	sx := -1
	if x0 < x1 {
		sx = 1
	}
	dy := -abs(y1 - y0)
	sy := -1
	if y0 < y1 {
		sy = 1
	}
	err := dx + dy
	for {
		if image.Pt(x0, y0).In(img.Bounds()) {
			img.SetNRGBA(x0, y0, c)
		}
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

func clamp(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
