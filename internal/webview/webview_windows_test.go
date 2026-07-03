//go:build windows && webview2

package webview

import "testing"

func TestClampRestoreSizeKeepsWindowBelowWorkArea(t *testing.T) {
	const workArea = 1920

	got := clampRestoreSize(1240, workArea)
	max := workArea * 88 / 100
	if got > max {
		t.Fatalf("restore size = %d, want at most %d", got, max)
	}
	if got == workArea {
		t.Fatalf("restore size should not fill the work area")
	}
}

func TestClampRestoreSizeHonorsComfortablePreferredSize(t *testing.T) {
	const (
		workArea  = 1500
		preferred = 1240
	)

	got := clampRestoreSize(preferred, workArea)
	if got != preferred {
		t.Fatalf("restore size = %d, want preferred size %d", got, preferred)
	}
}
