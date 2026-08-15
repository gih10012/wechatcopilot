package service

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/gih10012/wechatcopilot/internal/driver"
	"golang.org/x/sys/unix"
)

const (
	maxSendAttachments     = 20
	maxSendAttachmentBytes = int64(512 << 20)
	transactionSweepPeriod = 5 * time.Second
)

type preparedTransaction struct {
	Preview     PreparedSend
	Attachments []driver.Attachment
	StageDir    string
	Committing  bool
}

func (s *Service) initializeTransactionStaging() error {
	root := s.transactionStagingRoot()
	if err := ensurePrivateStagingDirectory(root); err != nil {
		return fmt.Errorf("initialize send transaction staging: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("list send transaction staging: %w", err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("remove abandoned send transaction staging: %w", err)
		}
	}
	return nil
}

func (s *Service) transactionStagingRoot() string {
	return filepath.Join(s.paths.Home, ".send-transactions")
}

func ensurePrivateStagingDirectory(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%s must be a directory, not a symlink", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return nil
}

func (s *Service) runTransactionReaper() {
	defer close(s.transactionDone)
	ticker := time.NewTicker(transactionSweepPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.transactionsMu.Lock()
			s.pruneTransactionsLocked()
			s.transactionsMu.Unlock()
		case <-s.transactionStop:
			return
		}
	}
}

func (s *Service) closeTransactions() error {
	var cleanupErr error
	s.transactionOnce.Do(func() {
		close(s.transactionStop)
		<-s.transactionDone
		s.transactionsMu.Lock()
		defer s.transactionsMu.Unlock()
		for id := range s.transactions {
			if err := s.cleanupTransactionLocked(id); err != nil {
				cleanupErr = errors.Join(cleanupErr, err)
			}
		}
	})
	return cleanupErr
}

func (s *Service) pruneTransactionsLocked() {
	now := time.Now().UTC()
	for id, transaction := range s.transactions {
		if !transaction.Committing && !now.Before(transaction.Preview.ExpiresAt) {
			_ = s.cleanupTransactionLocked(id)
		}
	}
}

func (s *Service) releaseTransactionCommit(id string) {
	s.transactionsMu.Lock()
	defer s.transactionsMu.Unlock()
	transaction, ok := s.transactions[id]
	if !ok {
		return
	}
	transaction.Committing = false
	s.transactions[id] = transaction
	if !time.Now().UTC().Before(transaction.Preview.ExpiresAt) {
		_ = s.cleanupTransactionLocked(id)
	}
}

func (s *Service) cleanupTransaction(id string) {
	s.transactionsMu.Lock()
	defer s.transactionsMu.Unlock()
	if err := s.cleanupTransactionLocked(id); err != nil {
		transaction, ok := s.transactions[id]
		if ok {
			transaction.Preview.ExpiresAt = time.Time{}
			s.transactions[id] = transaction
		}
	}
}

func (s *Service) cleanupTransactionLocked(id string) error {
	transaction, ok := s.transactions[id]
	if !ok {
		return nil
	}
	if transaction.StageDir != "" {
		if err := os.RemoveAll(transaction.StageDir); err != nil {
			return fmt.Errorf("remove send transaction staging: %w", err)
		}
	}
	delete(s.transactions, id)
	return nil
}

func (s *Service) stageSendAttachments(transactionID string, requested []driver.Attachment) ([]PreparedAttachment, []driver.Attachment, string, error) {
	if len(requested) == 0 {
		return nil, nil, "", nil
	}
	if len(requested) > maxSendAttachments {
		return nil, nil, "", fmt.Errorf("at most %d attachments are allowed", maxSendAttachments)
	}
	stageDir := filepath.Join(s.transactionStagingRoot(), transactionID)
	if err := ensurePrivateStagingDirectory(stageDir); err != nil {
		return nil, nil, "", err
	}
	cleanup := func(err error) ([]PreparedAttachment, []driver.Attachment, string, error) {
		if cleanupErr := os.RemoveAll(stageDir); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
		return nil, nil, "", err
	}

	previews := make([]PreparedAttachment, 0, len(requested))
	staged := make([]driver.Attachment, 0, len(requested))
	var total int64
	for index, attachment := range requested {
		path := filepath.Clean(attachment.LocalPath)
		if attachment.LocalPath == "" || !filepath.IsAbs(path) {
			return cleanup(errors.New("attachment local_path must be absolute"))
		}
		name := filepath.Base(path)
		if name == "." || name == string(filepath.Separator) || !utf8.ValidString(name) {
			return cleanup(fmt.Errorf("attachment %q has an invalid file name", path))
		}

		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return cleanup(fmt.Errorf("open attachment %q: %w", path, err))
		}
		source := os.NewFile(uintptr(fd), path)
		info, statErr := source.Stat()
		if statErr != nil {
			_ = source.Close()
			return cleanup(fmt.Errorf("inspect attachment %q: %w", path, statErr))
		}
		if !info.Mode().IsRegular() {
			_ = source.Close()
			return cleanup(fmt.Errorf("attachment %q must be a regular, non-symlink file", path))
		}
		remaining := maxSendAttachmentBytes - total
		if info.Size() > remaining {
			_ = source.Close()
			return cleanup(fmt.Errorf("attachments exceed %d bytes", maxSendAttachmentBytes))
		}

		destination := filepath.Join(stageDir, fmt.Sprintf("%02d.blob", index+1))
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = source.Close()
			return cleanup(fmt.Errorf("create staged attachment: %w", err))
		}
		digest := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(output, digest), io.LimitReader(source, remaining+1))
		closeSourceErr := source.Close()
		syncErr := output.Sync()
		closeOutputErr := output.Close()
		if copyErr != nil || closeSourceErr != nil || syncErr != nil || closeOutputErr != nil {
			return cleanup(errors.Join(copyErr, closeSourceErr, syncErr, closeOutputErr))
		}
		if written > remaining {
			return cleanup(fmt.Errorf("attachments exceed %d bytes", maxSendAttachmentBytes))
		}
		total += written
		digestHex := hex.EncodeToString(digest.Sum(nil))
		previews = append(previews, PreparedAttachment{Name: name, Size: written, SHA256: digestHex})
		staged = append(staged, driver.Attachment{
			Kind: "file", Name: name, Size: written, LocalPath: destination,
		})
	}
	if err := syncStagingDirectory(stageDir); err != nil {
		return cleanup(err)
	}
	return previews, staged, stageDir, nil
}

func syncStagingDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func canonicalSendHash(conversationID, text string, attachments []PreparedAttachment, shareSurfaceID string) string {
	digest := sha256.New()
	writeHashField := func(value string) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = digest.Write(length[:])
		_, _ = digest.Write([]byte(value))
	}
	writeHashField(conversationID)
	writeHashField(text)
	writeHashField(shareSurfaceID)
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(attachments)))
	_, _ = digest.Write(count[:])
	for _, attachment := range attachments {
		writeHashField(attachment.Name)
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(attachment.Size))
		_, _ = digest.Write(size[:])
		writeHashField(attachment.SHA256)
	}
	return hex.EncodeToString(digest.Sum(nil))
}
