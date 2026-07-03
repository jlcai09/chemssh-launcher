package fileicon

import (
	"container/list"
	"errors"
	"path/filepath"
	"strings"
	"sync"
)

var ErrUnsupported = errors.New("system file icons are not supported on this platform")

const (
	defaultCacheSize = 500 // 缓存 500 个图标，约 5-10MB
)

type cacheEntry struct {
	key  string
	data []byte
}

type Service struct {
	mu      sync.Mutex
	cache   map[string]*list.Element
	lruList *list.List
	maxSize int
	failed  map[string]bool // negative cache: keys that failed to load
	loadMu  sync.Mutex      // serialises Windows SHGetFileInfoW calls for thread safety
	hits    int64           // cache hit counter
	misses  int64           // cache miss counter
}

func NewService() *Service {
	return &Service{
		cache:   make(map[string]*list.Element),
		lruList: list.New(),
		maxSize: defaultCacheSize,
		failed:  make(map[string]bool),
	}
}

// CacheStats returns cache hit rate for monitoring
func (s *Service) CacheStats() (hits, misses int64, size int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits, s.misses, s.lruList.Len()
}

func (s *Service) IconPNG(name string, isDir bool, size int) ([]byte, error) {
	size = normalizeSize(size)
	key := cacheKey(name, isDir, size)

	// Try to get from cache.
	// NOTE: A plain read lock is not safe here because MoveToFront and hits++
	// are both writes; using a full mutex avoids linked-list corruption when
	// multiple goroutines hit the cache concurrently.
	s.mu.Lock()
	if elem, ok := s.cache[key]; ok {
		s.hits++
		s.lruList.MoveToFront(elem)
		data := elem.Value.(*cacheEntry).data
		s.mu.Unlock()
		return data, nil
	}
	wasFailed := s.failed[key]
	s.mu.Unlock()

	// Cache miss
	s.mu.Lock()
	s.misses++
	s.mu.Unlock()

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

	// Add to LRU cache
	s.addToCache(key, data)
	return data, nil
}

// addToCache adds an entry to the LRU cache
func (s *Service) addToCache(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// If key already exists, move to front
	if elem, ok := s.cache[key]; ok {
		s.lruList.MoveToFront(elem)
		elem.Value.(*cacheEntry).data = data
		return
	}

	// Add new entry
	entry := &cacheEntry{key: key, data: data}
	elem := s.lruList.PushFront(entry)
	s.cache[key] = elem

	// Evict oldest if cache is full
	if s.lruList.Len() > s.maxSize {
		oldest := s.lruList.Back()
		if oldest != nil {
			s.lruList.Remove(oldest)
			oldEntry := oldest.Value.(*cacheEntry)
			delete(s.cache, oldEntry.key)
		}
	}
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
