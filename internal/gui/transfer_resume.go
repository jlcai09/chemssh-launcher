package gui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"chemssh-launcher/internal/sftpclient"

	"github.com/pkg/sftp"
)

const (
	transferMaxAttempts   = 5
	transferRetryMaxDelay = 15 * time.Second
)

var transferRetryBaseDelay = time.Second

var errTransferSourceChanged = errors.New("source file changed during the transfer; restart it to continue")

// errTransferUnsupported marks failures that cannot be fixed by retrying,
// such as transferring a directory.
var errTransferUnsupported = errors.New("transfer is not supported")

type resumableSource interface {
	Stat() (size int64, modTime time.Time, err error)
	OpenAt(offset int64) (io.ReadCloser, error)
	Reconnect() error
}

type resumableTarget interface {
	Size() (size int64, exists bool, err error)
	OpenAt(offset int64) (io.WriteCloser, error)
	Reconnect() error
}

type localFileSource struct {
	path string
}

func newLocalFileSource(path string) *localFileSource {
	return &localFileSource{path: localCleanPath(path)}
}

func (s *localFileSource) Stat() (int64, time.Time, error) {
	info, err := os.Stat(s.path)
	if err != nil {
		return 0, time.Time{}, err
	}
	if info.IsDir() {
		return 0, time.Time{}, fmt.Errorf("%w: %s is a directory", errTransferUnsupported, s.path)
	}
	return info.Size(), info.ModTime(), nil
}

func (s *localFileSource) OpenAt(offset int64) (io.ReadCloser, error) {
	file, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return file, nil
}

func (s *localFileSource) Reconnect() error { return nil }

type localFileTarget struct {
	path string
}

func newLocalFileTarget(path string) *localFileTarget {
	return &localFileTarget{path: path}
}

func (t *localFileTarget) Size() (int64, bool, error) {
	info, err := os.Stat(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if info.IsDir() {
		return 0, false, fmt.Errorf("%w: target %s is a directory", errTransferUnsupported, t.path)
	}
	return info.Size(), true, nil
}

func (t *localFileTarget) OpenAt(offset int64) (io.WriteCloser, error) {
	if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(t.path, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return nil, err
	}
	if err := file.Truncate(offset); err != nil {
		_ = file.Close()
		return nil, err
	}
	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return file, nil
}

func (t *localFileTarget) Reconnect() error { return nil }

type sftpFileSource struct {
	manager *sftpclient.Manager
	session string
	path    string
}

func (s *sftpFileSource) Stat() (int64, time.Time, error) {
	info, err := s.manager.Stat(s.session, s.path)
	if err != nil {
		return 0, time.Time{}, err
	}
	return info.Size, info.ModTime, nil
}

func (s *sftpFileSource) OpenAt(offset int64) (io.ReadCloser, error) {
	reader, _, err := s.manager.OpenReadAt(s.session, s.path, offset)
	return reader, err
}

func (s *sftpFileSource) Reconnect() error {
	return s.manager.Reconnect(s.session)
}

type sftpFileTarget struct {
	manager *sftpclient.Manager
	session string
	path    string
}

func (t *sftpFileTarget) Size() (int64, bool, error) {
	info, err := t.manager.Stat(t.session, t.path)
	if err != nil {
		if isRemoteNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return info.Size, true, nil
}

func (t *sftpFileTarget) OpenAt(offset int64) (io.WriteCloser, error) {
	return t.manager.OpenWriteAt(t.session, t.path, offset)
}

func (t *sftpFileTarget) Reconnect() error {
	return t.manager.Reconnect(t.session)
}

func isRemoteNotExist(err error) bool {
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	var status *sftp.StatusError
	if errors.As(err, &status) {
		return status.FxCode() == sftp.ErrSSHFxNoSuchFile
	}
	return false
}

func isRetryableTransferError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errTransferSourceChanged) ||
		errors.Is(err, errTransferUnsupported) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, os.ErrPermission) ||
		errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ENOSPC) {
		return false
	}
	var status *sftp.StatusError
	if errors.As(err, &status) {
		switch status.FxCode() {
		case sftp.ErrSSHFxPermissionDenied, sftp.ErrSSHFxNoSuchFile, sftp.ErrSSHFxOpUnsupported:
			return false
		}
	}
	message := strings.ToLower(err.Error())
	return !strings.Contains(message, "transfer cancelled")
}

func transferRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := transferRetryBaseDelay << (attempt - 1)
	if delay <= 0 || delay > transferRetryMaxDelay {
		delay = transferRetryMaxDelay
	}
	return delay
}

// runResumableTransfer copies src to dst and survives transport failures: each
// retry reopens both sides at the last confirmed byte offset instead of
// restarting, and re-establishes dropped SSH connections first.
func (s *Server) runResumableTransfer(id string, resume bool, displayName string, src resumableSource, dst resumableTarget) error {
	offset, err := s.transfers.begin(id, 0, resume)
	if err != nil {
		return err
	}
	storedSize, storedModTime, hasStoredIdentity := s.transfers.sourceIdentity(id)
	if !resume {
		offset = 0
		s.transfers.setLoaded(id, 0)
	}

	for attempt := 1; attempt <= transferMaxAttempts; attempt++ {
		size, modTime, err := src.Stat()
		if err == nil {
			switch {
			case attempt > 1:
				if hasStoredIdentity && (size != storedSize || !modTime.Equal(storedModTime)) {
					err = errTransferSourceChanged
				}
			case !hasStoredIdentity:
				storedSize, storedModTime, hasStoredIdentity = size, modTime, true
				s.transfers.setSourceIdentity(id, size, modTime)
			case size != storedSize || !modTime.Equal(storedModTime):
				if resume {
					// A manual retry may resume a different version of the file;
					// restarting is safe, silently appending would corrupt it.
					s.logs.add("transfer source changed, restarting: " + displayName)
					offset = 0
					s.transfers.setLoaded(id, 0)
				}
				storedSize, storedModTime = size, modTime
				s.transfers.setSourceIdentity(id, size, modTime)
			}
			if err == nil {
				s.transfers.setTotal(id, size)
			}
		}

		if err == nil {
			offset, err = s.resolveTargetOffset(id, offset, dst)
		}
		if err == nil {
			err = s.copyResumableAt(id, offset, src, dst)
		}
		if err == nil {
			progress, _ := s.transfers.snapshot(id)
			if progress.Total > 0 && progress.Loaded < progress.Total {
				err = errTransferSourceChanged
			}
		}
		if err == nil {
			if sizeAfter, modAfter, statErr := src.Stat(); statErr == nil &&
				(sizeAfter != storedSize || !modAfter.Equal(storedModTime)) {
				err = errTransferSourceChanged
			}
		}
		if err == nil {
			s.finishProgress(id, nil)
			return nil
		}
		if errors.Is(err, context.Canceled) || !isRetryableTransferError(err) || attempt >= transferMaxAttempts {
			s.finishProgress(id, err)
			return err
		}

		if reconnectErr := src.Reconnect(); reconnectErr != nil {
			s.logs.add("transfer reconnect failed for " + displayName + ": " + reconnectErr.Error())
		}
		if reconnectErr := dst.Reconnect(); reconnectErr != nil {
			s.logs.add("transfer reconnect failed for " + displayName + ": " + reconnectErr.Error())
		}

		offset = s.transfers.loaded(id)
		delay := transferRetryDelay(attempt)
		s.transfers.retryScheduled(id, attempt+1, transferMaxAttempts, time.Now().Add(delay))
		s.logs.add(fmt.Sprintf("transfer retry %d/%d for %s: %v", attempt+1, transferMaxAttempts, displayName, err))
		if waitErr := s.waitTransferRetry(id, delay); waitErr != nil {
			s.finishProgress(id, waitErr)
			return waitErr
		}
	}
	return nil
}

func (s *Server) resolveTargetOffset(id string, offset int64, dst resumableTarget) (int64, error) {
	if offset <= 0 {
		return 0, nil
	}
	size, exists, err := dst.Size()
	if err != nil {
		return offset, err
	}
	if !exists {
		offset = 0
	} else if size < offset {
		offset = size
	}
	if offset < s.transfers.loaded(id) {
		s.transfers.setLoaded(id, offset)
	}
	return offset, nil
}

func (s *Server) copyResumableAt(id string, offset int64, src resumableSource, dst resumableTarget) (err error) {
	reader, err := src.OpenAt(offset)
	if err != nil {
		return err
	}
	writer, err := dst.OpenAt(offset)
	if err != nil {
		_ = reader.Close()
		return err
	}
	defer func() {
		_ = reader.Close()
		if closeErr := writer.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	buffer := make([]byte, copyBufferSize)
	for {
		if err := s.transfers.waitAllowed(id); err != nil {
			return err
		}
		n, readErr := reader.Read(buffer)
		if n > 0 {
			written := 0
			for written < n {
				if err := s.transfers.waitAllowed(id); err != nil {
					return err
				}
				count, writeErr := writer.Write(buffer[written:n])
				if count > 0 {
					written += count
					s.transfers.add(id, count)
				}
				if writeErr != nil {
					return writeErr
				}
				if count == 0 {
					return io.ErrShortWrite
				}
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func (s *Server) waitTransferRetry(id string, delay time.Duration) error {
	deadline := time.Now().Add(delay)
	for {
		if err := s.transfers.waitAllowed(id); err != nil {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		if remaining > 250*time.Millisecond {
			remaining = 250 * time.Millisecond
		}
		time.Sleep(remaining)
	}
}
