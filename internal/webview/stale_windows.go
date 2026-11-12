//go:build windows

package webview

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	processCommandLineInformation = 60
	statusInfoLengthMismatch      = 0xC0000004
)

var (
	ntdll                         = syscall.NewLazyDLL("ntdll.dll")
	procNtQueryInformationProcess = ntdll.NewProc("NtQueryInformationProcess")
)

type unicodeString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

type processRecord struct {
	pid     uint32
	parent  uint32
	exe     string
	created time.Time
}

// cleanupStaleWebViewProcesses terminates WebView2 processes that use this
// launcher's data directory after their host process has exited. Such an
// orphan keeps the profile locked, which makes every later startup hang before
// the first page load.
func cleanupStaleWebViewProcesses(dataPath string) ([]uint32, error) {
	records, commandLines, err := snapshotProcesses()
	if err != nil {
		return nil, err
	}
	stale := staleWebViewProcesses(records, commandLines, dataPath)
	killed := make([]uint32, 0, len(stale))
	var failures []error
	for _, pid := range stale {
		if err := terminateProcess(pid); err != nil {
			// Children of a suspended browser can already be terminating when
			// the sweep reaches them; do not report those as failures.
			if errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
				(errors.Is(err, windows.ERROR_ACCESS_DENIED) && waitForProcessExit(pid, 1500*time.Millisecond)) {
				continue
			}
			failures = append(failures, fmt.Errorf("terminate %d: %w", pid, err))
			continue
		}
		killed = append(killed, pid)
	}
	return killed, errors.Join(failures...)
}

func snapshotProcesses() (map[uint32]processRecord, map[uint32]string, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, nil, err
	}
	defer windows.CloseHandle(snapshot)

	records := make(map[uint32]processRecord)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		records[entry.ProcessID] = processRecord{
			pid:    entry.ProcessID,
			parent: entry.ParentProcessID,
			exe:    windows.UTF16ToString(entry.ExeFile[:]),
		}
	}
	if err != nil && !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, nil, err
	}

	commandLines := make(map[uint32]string)
	for pid, record := range records {
		if !strings.EqualFold(record.exe, "msedgewebview2.exe") {
			continue
		}
		handle, openErr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if openErr != nil {
			continue
		}
		if commandLine, cmdErr := processCommandLine(handle); cmdErr == nil {
			commandLines[pid] = commandLine
		}
		if created, timeErr := processCreationTime(handle); timeErr == nil {
			record.created = created
			records[pid] = record
		}
		windows.CloseHandle(handle)
	}

	// A reused parent PID looks alive even though the original host is gone.
	// Its creation time is later than the browser it supposedly started.
	for _, record := range records {
		if !strings.EqualFold(record.exe, "msedgewebview2.exe") {
			continue
		}
		parent, ok := records[record.parent]
		if !ok || !parent.created.IsZero() {
			continue
		}
		handle, openErr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, parent.pid)
		if openErr != nil {
			continue
		}
		if created, timeErr := processCreationTime(handle); timeErr == nil {
			parent.created = created
			records[parent.pid] = parent
		}
		windows.CloseHandle(handle)
	}

	return records, commandLines, nil
}

func processCommandLine(handle windows.Handle) (string, error) {
	size := uint32(64 * 1024)
	for attempt := 0; attempt < 2; attempt++ {
		buffer := make([]uint16, size/2)
		var returned uint32
		status, _, _ := procNtQueryInformationProcess.Call(
			uintptr(handle),
			uintptr(processCommandLineInformation),
			uintptr(unsafe.Pointer(&buffer[0])),
			uintptr(len(buffer)*2),
			uintptr(unsafe.Pointer(&returned)),
		)
		if status == 0 {
			if returned < uint32(unsafe.Sizeof(unicodeString{})) {
				return "", nil
			}
			value := (*unicodeString)(unsafe.Pointer(&buffer[0]))
			if value.Buffer == nil || value.Length < 2 {
				return "", nil
			}
			return windows.UTF16ToString(unsafe.Slice(value.Buffer, int(value.Length/2))), nil
		}
		if uint32(status) != statusInfoLengthMismatch || returned == 0 {
			return "", fmt.Errorf("NtQueryInformationProcess: status 0x%x", uint32(status))
		}
		size = returned + 16
	}
	return "", errors.New("process command line is too long")
}

func processCreationTime(handle windows.Handle) (time.Time, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, creation.Nanoseconds()), nil
}

func terminateProcess(pid uint32) error {
	const access = windows.PROCESS_TERMINATE | windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.SYNCHRONIZE
	handle, err := windows.OpenProcess(access, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := windows.TerminateProcess(handle, 1); err != nil {
		return err
	}
	// Do not race the new WebView2 environment against a profile that is still
	// being released by the old browser process.
	_, _ = windows.WaitForSingleObject(handle, 3000)
	return nil
}

func waitForProcessExit(pid uint32, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !processRunning(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func processRunning(pid uint32) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(handle)
	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return true
	}
	return exitCode == 259 // STILL_ACTIVE
}

// staleWebViewProcesses returns the PIDs of WebView2 processes that belong to
// dataPath and whose process tree is no longer owned by a live host.
func staleWebViewProcesses(records map[uint32]processRecord, commandLines map[uint32]string, dataPath string) []uint32 {
	if dataPath == "" {
		return nil
	}
	candidates := make(map[uint32]bool)
	for pid, record := range records {
		if !strings.EqualFold(record.exe, "msedgewebview2.exe") {
			continue
		}
		if commandLineUsesDataDir(commandLines[pid], dataPath) {
			candidates[pid] = true
		}
	}
	stale := make([]uint32, 0, len(candidates))
	for pid := range candidates {
		if candidateTreeIsOrphaned(records, candidates, pid) {
			stale = append(stale, pid)
		}
	}
	// Kill the browser process before its children so Chromium cannot restart
	// them while the sweep is running.
	sort.SliceStable(stale, func(i, j int) bool {
		rootI, rootJ := isCandidateRoot(records, candidates, stale[i]), isCandidateRoot(records, candidates, stale[j])
		if rootI != rootJ {
			return rootI
		}
		return stale[i] < stale[j]
	})
	return stale
}

func candidateTreeIsOrphaned(records map[uint32]processRecord, candidates map[uint32]bool, pid uint32) bool {
	seen := make(map[uint32]bool)
	for {
		if seen[pid] {
			return false
		}
		seen[pid] = true
		record, ok := records[pid]
		if !ok {
			return false
		}
		parentID := record.parent
		if parentID == 0 || parentID == pid {
			return true
		}
		parent, ok := records[parentID]
		if !ok {
			return true
		}
		if !record.created.IsZero() && !parent.created.IsZero() && parent.created.After(record.created) {
			return true
		}
		if !candidates[parentID] {
			return false
		}
		pid = parentID
	}
}

func isCandidateRoot(records map[uint32]processRecord, candidates map[uint32]bool, pid uint32) bool {
	record, ok := records[pid]
	if !ok {
		return true
	}
	return !candidates[record.parent]
}

func commandLineUsesDataDir(commandLine, dataPath string) bool {
	dir := normalizeDataPath(dataPath)
	if dir == "" {
		return false
	}
	command := strings.ToLower(strings.ReplaceAll(commandLine, "/", `\`))
	for offset := 0; offset < len(command); {
		index := strings.Index(command[offset:], dir)
		if index < 0 {
			return false
		}
		index += offset
		end := index + len(dir)
		if end == len(command) {
			return true
		}
		switch command[end] {
		case '\\', '"', '\'':
			return true
		}
		offset = end
	}
	return false
}

func normalizeDataPath(path string) string {
	path = strings.ReplaceAll(path, "/", `\`)
	return strings.ToLower(strings.TrimRight(path, `\`))
}
