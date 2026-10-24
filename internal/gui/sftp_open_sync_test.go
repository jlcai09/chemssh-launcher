package gui

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckSFTPOpenSyncFileSize(t *testing.T) {
	tests := []struct {
		name    string
		size    int64
		wantErr bool
	}{
		{name: "below limit", size: sftpOpenSyncMaxFileSize - 1},
		{name: "at limit", size: sftpOpenSyncMaxFileSize},
		{name: "above limit", size: sftpOpenSyncMaxFileSize + 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkSFTPOpenSyncFileSize(tt.size)
			if tt.wantErr {
				if !errors.Is(err, errSFTPOpenFileTooLarge) {
					t.Fatalf("checkSFTPOpenSyncFileSize(%d) error = %v, want errSFTPOpenFileTooLarge", tt.size, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkSFTPOpenSyncFileSize(%d) error = %v, want nil", tt.size, err)
			}
		})
	}
}

func TestSFTPOpenSyncRegisterRejectsLargeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.bin")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(sftpOpenSyncMaxFileSize + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	manager := &sftpOpenSyncManager{items: map[string]*sftpOpenSyncItem{}}
	err = manager.Register("session", "/remote/large.bin", path)
	if !errors.Is(err, errSFTPOpenFileTooLarge) {
		t.Fatalf("Register() error = %v, want errSFTPOpenFileTooLarge", err)
	}
}
