package fileicon

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
)

var ErrUnsupported = errors.New("system file icons are not supported on this platform")

type Service struct {
	mu     sync.RWMutex
	cache  map[string][]byte
	failed map[string]bool // negative cache: keys that failed to load
	loadMu sync.Mutex      // serialises Windows SHGetFileInfoW calls for thread safety
}

func NewService() *Service {
	return &Service{
		cache:  make(map[string][]byte),
		failed: make(map[string]bool),
	}
}

func (s *Service) IconPNG(name string, isDir bool, size int) ([]byte, error) {
	size = normalizeSize(size)
	key := cacheKey(name, isDir, size)

	s.mu.RLock()
	data, ok := s.cache[key]
	wasFailed := s.failed[key]
	s.mu.RUnlock()
	if ok {
		return data, nil
	}
	// If this key previously failed to load from the system, go straight to fallback.
	if wasFailed {
		return fallbackIconPNG(isDir, size)
	}

	// Serialize Windows shell API calls to avoid GDI/COM concurrency issues.
	s.loadMu.Lock()
	data, err := loadSystemIconPNG(lookupName(name, isDir), isDir, size)
	s.loadMu.Unlock()

	if err != nil {
		if !isDir {
			s.loadMu.Lock()
			data, err = loadSystemIconPNG("file", false, size)
			s.loadMu.Unlock()
		}
		if err != nil {
			// Remember this failure so we skip the system call next time.
			s.mu.Lock()
			s.failed[key] = true
			s.mu.Unlock()
			data, err = fallbackIconPNG(isDir, size)
			if err != nil {
				return nil, err
			}
		}
	}

	s.mu.Lock()
	s.cache[key] = data
	s.mu.Unlock()
	return data, nil
}

func normalizeSize(size int) int {
	if size <= 16 {
		return 16
	}
	return 32
}

func cacheKey(name string, isDir bool, size int) string {
	if isDir {
		return "dir:" + strconvSize(size)
	}
	ext := normalizedExt(name)
	if ext == "" {
		return "file:" + strconvSize(size)
	}
	return ext + ":" + strconvSize(size)
}

func lookupName(name string, isDir bool) string {
	if isDir {
		return "folder"
	}
	ext := normalizedExt(name)
	if ext == "" {
		return "file"
	}
	return "file" + ext
}

func normalizedExt(name string) string {
	clean := strings.TrimSpace(name)
	if clean == "" {
		return ""
	}
	if strings.HasPrefix(clean, ".") && strings.Count(clean, ".") == 1 {
		return strings.ToLower(clean)
	}
	return strings.ToLower(filepath.Ext(clean))
}

func strconvSize(size int) string {
	if size <= 16 {
		return "16"
	}
	return "32"
}
