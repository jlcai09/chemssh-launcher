//go:build !windows

package trash

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

func Move(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		return moveToDarwinTrash(absolute)
	}
	return moveToFreedesktopTrash(absolute)
}

func moveToDarwinTrash(path string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	trashDir := filepath.Join(home, ".Trash")
	if err := os.MkdirAll(trashDir, 0o700); err != nil {
		return err
	}
	return os.Rename(path, uniquePath(trashDir, filepath.Base(path)))
}

func moveToFreedesktopTrash(path string) error {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dataHome = filepath.Join(home, ".local", "share")
	}
	filesDir := filepath.Join(dataHome, "Trash", "files")
	infoDir := filepath.Join(dataHome, "Trash", "info")
	if err := os.MkdirAll(filesDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(infoDir, 0o700); err != nil {
		return err
	}
	target := uniquePath(filesDir, filepath.Base(path))
	infoPath := filepath.Join(infoDir, filepath.Base(target)+".trashinfo")
	info := fmt.Sprintf("[Trash Info]\nPath=%s\nDeletionDate=%s\n", url.PathEscape(path), time.Now().Format("2006-01-02T15:04:05"))
	if err := os.WriteFile(infoPath, []byte(info), 0o600); err != nil {
		return err
	}
	if err := os.Rename(path, target); err != nil {
		os.Remove(infoPath)
		return err
	}
	return nil
}

func uniquePath(dir string, name string) string {
	candidate := filepath.Join(dir, name)
	for index := 1; ; index++ {
		if _, err := os.Lstat(candidate); err != nil {
			return candidate
		}
		candidate = filepath.Join(dir, fmt.Sprintf("%s.%d", name, index))
	}
}
