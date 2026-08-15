package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

var ErrStateLocked = errors.New("another daemon already owns this state home")

// StateLock prevents independent daemon sockets from operating on one registry
// and its account runtimes at the same time.
type StateLock struct {
	file *os.File
	once sync.Once
	err  error
}

func AcquireStateLock(stateHome string) (*StateLock, error) {
	if !filepath.IsAbs(stateHome) {
		return nil, errors.New("daemon state home must be absolute")
	}
	path := filepath.Join(filepath.Clean(stateHome), ".daemon.lock")
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open daemon state lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	closeOnError := func(err error) (*StateLock, error) {
		_ = file.Close()
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return closeOnError(fmt.Errorf("inspect daemon state lock: %w", err))
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != uint32(os.Geteuid()) {
		return closeOnError(errors.New("daemon state lock must be a regular file owned by the current user"))
	}
	if err := file.Chmod(0o600); err != nil {
		return closeOnError(fmt.Errorf("protect daemon state lock: %w", err))
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return closeOnError(ErrStateLocked)
		}
		return closeOnError(fmt.Errorf("lock daemon state home: %w", err))
	}
	return &StateLock{file: file}, nil
}

func (l *StateLock) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
		l.err = errors.Join(unlockErr, l.file.Close())
	})
	return l.err
}
