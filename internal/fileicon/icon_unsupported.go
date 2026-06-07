//go:build !windows

package fileicon

func loadSystemIconPNG(name string, isDir bool, size int) ([]byte, error) {
	return nil, ErrUnsupported
}
