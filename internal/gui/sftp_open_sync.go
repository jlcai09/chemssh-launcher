package gui

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"chemssh-launcher/internal/sftpclient"

	"github.com/fsnotify/fsnotify"
)

const (
	sftpOpenSyncDebounce   = 500 * time.Millisecond // 防抖时间：快速响应
	sftpOpenSyncRetryDelay = 3 * time.Second
)

type sftpOpenSyncManager struct {
	mu       sync.Mutex
	items    map[string]*sftpOpenSyncItem
	sftp     *sftpclient.Manager
	logs     *safeLog
	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	closed   bool
	nextSeq  int64
	events   []sftpOpenSyncEvent
	watcher  *fsnotify.Watcher // 文件系统监控
}

type sftpOpenSyncItem struct {
	sessionID  string
	remotePath string
	localPath  string
	modTime    time.Time
	size       int64
	dirty      bool
	syncing    bool
	nextSync   time.Time
}

type sftpOpenSyncUpload struct {
	sessionID  string
	remotePath string
	localPath  string
	modTime    time.Time
	size       int64
}

type sftpOpenSyncEvent struct {
	Seq        int64     `json:"seq"`
	Time       time.Time `json:"time"`
	Session    string    `json:"session"`
	RemotePath string    `json:"remote_path"`
	LocalPath  string    `json:"local_path"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
}

func newSFTPOpenSyncManager(sftp *sftpclient.Manager, logs *safeLog) *sftpOpenSyncManager {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		// 如果 fsnotify 初始化失败，记录错误但继续（会降级到定期检查）
		if logs != nil {
			logs.add("SFTP open sync: fsnotify initialization failed, using periodic check: " + err.Error())
		}
	}

	m := &sftpOpenSyncManager{
		items:   map[string]*sftpOpenSyncItem{},
		sftp:    sftp,
		logs:    logs,
		done:    make(chan struct{}),
		watcher: watcher,
	}
	go m.loop()
	return m
}

func (m *sftpOpenSyncManager) Register(sessionID, remotePath, localPath string) error {
	abs, err := filepath.Abs(localPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return errors.New("cannot sync a directory cache entry")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// 如果已存在，先从 watcher 移除
	if _, exists := m.items[abs]; exists && m.watcher != nil {
		_ = m.watcher.Remove(abs)
	}

	m.items[abs] = &sftpOpenSyncItem{
		sessionID:  sessionID,
		remotePath: remotePath,
		localPath:  abs,
		modTime:    info.ModTime(),
		size:       info.Size(),
	}

	// 添加到 fsnotify watcher
	if m.watcher != nil {
		if err := m.watcher.Add(abs); err != nil {
			if m.logs != nil {
				m.logs.add("SFTP open sync: failed to watch " + abs + ": " + err.Error())
			}
		}
	}

	count := len(m.items)

	if m.logs != nil {
		m.logs.add("SFTP open sync watching: " + abs + " -> " + remotePath)
		if count == 1 {
			m.logs.add("SFTP open sync started")
		}
	}
	return nil
}

func (m *sftpOpenSyncManager) UnregisterSession(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	removed := 0
	for path, item := range m.items {
		if item.sessionID == sessionID {
			// 从 fsnotify watcher 移除
			if m.watcher != nil {
				_ = m.watcher.Remove(path)
			}
			delete(m.items, path)
			removed++
		}
	}

	if removed > 0 && m.logs != nil {
		m.logs.add("SFTP open sync stopped for disconnected session")
	}
}

func (m *sftpOpenSyncManager) CloseAll() {
	m.stopOnce.Do(func() {
		close(m.done)
	})
	m.mu.Lock()
	m.closed = true
	m.items = map[string]*sftpOpenSyncItem{}
	if m.watcher != nil {
		_ = m.watcher.Close()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *sftpOpenSyncManager) loop() {
	// 如果 fsnotify 可用，使用事件驱动模式
	if m.watcher != nil {
		m.eventDrivenLoop()
	} else {
		// 降级到轮询模式
		m.pollingLoop()
	}
}

// eventDrivenLoop 使用 fsnotify 事件驱动
func (m *sftpOpenSyncManager) eventDrivenLoop() {
	// 使用较短的 ticker 检查需要同步的文件
	checkTicker := time.NewTicker(200 * time.Millisecond)
	defer checkTicker.Stop()

	for {
		select {
		case event, ok := <-m.watcher.Events:
			if !ok {
				return
			}
			// 只处理写入和重命名事件
			if event.Op&(fsnotify.Write|fsnotify.Rename) != 0 {
				m.handleFileChange(event.Name)
			}
			// 处理删除事件
			if event.Op&fsnotify.Remove != 0 {
				m.handleFileRemove(event.Name)
			}

		case err, ok := <-m.watcher.Errors:
			if !ok {
				return
			}
			if m.logs != nil {
				m.logs.add("SFTP open sync watcher error: " + err.Error())
			}

		case <-checkTicker.C:
			// 频繁检查是否有文件需要同步（防抖时间到了）
			m.checkAllFiles()

		case <-m.done:
			return
		}
	}
}

// pollingLoop 降级到轮询模式（fsnotify 不可用时）
func (m *sftpOpenSyncManager) pollingLoop() {
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.poll()
		case <-m.done:
			return
		}
	}
}

// handleFileChange 处理文件变更事件
func (m *sftpOpenSyncManager) handleFileChange(path string) {
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return
	}

	item := m.items[path]
	if item == nil || item.syncing {
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		return
	}

	if info.IsDir() {
		return
	}

	// 检测到文件变更
	if !info.ModTime().Equal(item.modTime) || info.Size() != item.size {
		item.modTime = info.ModTime()
		item.size = info.Size()
		item.dirty = true
		item.nextSync = now.Add(sftpOpenSyncDebounce)
	}
}

// handleFileRemove 处理文件删除事件
func (m *sftpOpenSyncManager) handleFileRemove(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return
	}

	if _, exists := m.items[path]; exists {
		delete(m.items, path)
		if m.watcher != nil {
			_ = m.watcher.Remove(path)
		}
		if m.logs != nil {
			m.logs.add("SFTP open sync stopped for removed cache file: " + path)
		}
	}
}

// checkAllFiles 检查所有文件是否需要同步
func (m *sftpOpenSyncManager) checkAllFiles() {
	now := time.Now()
	var uploads []sftpOpenSyncUpload

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}

	for _, item := range m.items {
		if item.syncing {
			continue
		}

		// 检查文件是否需要同步
		if item.dirty && !now.Before(item.nextSync) {
			item.syncing = true
			uploads = append(uploads, sftpOpenSyncUpload{
				sessionID:  item.sessionID,
				remotePath: item.remotePath,
				localPath:  item.localPath,
				modTime:    item.modTime,
				size:       item.size,
			})
		}
	}
	m.wg.Add(len(uploads))
	m.mu.Unlock()

	for _, upload := range uploads {
		go m.upload(upload)
	}
}

func (m *sftpOpenSyncManager) poll() {
	now := time.Now()
	var uploads []sftpOpenSyncUpload

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	for path, item := range m.items {
		if item.syncing {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				delete(m.items, path)
				if m.logs != nil {
					m.logs.add("SFTP open sync stopped for removed cache file: " + path)
				}
			}
			continue
		}
		if info.IsDir() {
			continue
		}
		if !info.ModTime().Equal(item.modTime) || info.Size() != item.size {
			item.modTime = info.ModTime()
			item.size = info.Size()
			item.dirty = true
			item.nextSync = now.Add(sftpOpenSyncDebounce)
		}
		if item.dirty && !now.Before(item.nextSync) {
			item.syncing = true
			uploads = append(uploads, sftpOpenSyncUpload{
				sessionID:  item.sessionID,
				remotePath: item.remotePath,
				localPath:  item.localPath,
				modTime:    item.modTime,
				size:       item.size,
			})
		}
	}
	m.wg.Add(len(uploads))
	m.mu.Unlock()

	for _, upload := range uploads {
		go m.upload(upload)
	}
}

func (m *sftpOpenSyncManager) upload(upload sftpOpenSyncUpload) {
	defer m.wg.Done()

	file, err := os.Open(upload.localPath)
	if err == nil {
		err = m.sftp.UploadFile(upload.sessionID, upload.remotePath, file)
	}
	if file != nil {
		_ = file.Close()
	}

	m.mu.Lock()
	item := m.items[upload.localPath]
	if item != nil {
		item.syncing = false
		if err == nil {
			if item.modTime.Equal(upload.modTime) && item.size == upload.size {
				item.dirty = false
			}
		} else if errors.Is(err, sftpclient.ErrSessionNotFound) {
			delete(m.items, upload.localPath)
		} else {
			item.dirty = true
			item.nextSync = time.Now().Add(sftpOpenSyncRetryDelay)
		}
	}
	m.mu.Unlock()

	status := "done"
	errorText := ""
	if err != nil {
		status = "error"
		errorText = err.Error()
	}
	m.addEvent(sftpOpenSyncEvent{
		Session:    upload.sessionID,
		RemotePath: upload.remotePath,
		LocalPath:  upload.localPath,
		Status:     status,
		Error:      errorText,
	})

	if m.logs == nil {
		return
	}
	if err != nil {
		m.logs.add("SFTP open sync failed: " + upload.localPath + " -> " + upload.remotePath + ": " + err.Error())
		return
	}
	m.logs.add("SFTP open sync uploaded: " + upload.localPath + " -> " + upload.remotePath)
}

func (m *sftpOpenSyncManager) EventsSince(seq int64) []sftpOpenSyncEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	events := make([]sftpOpenSyncEvent, 0, len(m.events))
	for _, event := range m.events {
		if event.Seq > seq {
			events = append(events, event)
		}
	}
	return events
}

func (m *sftpOpenSyncManager) addEvent(event sftpOpenSyncEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextSeq++
	event.Seq = m.nextSeq
	event.Time = time.Now()
	m.events = append(m.events, event)
	if len(m.events) > 100 {
		m.events = m.events[len(m.events)-100:]
	}
}
