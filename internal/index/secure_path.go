package index

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const indexFilename = "index.sqlite3"

type fileIdentity struct {
	device uint64
	inode  uint64
}

type pinnedIndexPath struct {
	directory *os.File
	file      *os.File
	dirID     fileIdentity
	fileID    fileIdentity
	dsn       string
	created   bool
}

func (p *pinnedIndexPath) close() error {
	if p == nil {
		return nil
	}
	var result error
	if p.file != nil {
		result = errors.Join(result, p.file.Close())
		p.file = nil
	}
	if p.directory != nil {
		result = errors.Join(result, p.directory.Close())
		p.directory = nil
	}
	return result
}

func pinIndexPath(path, accountID string, createIfMissing bool) (*pinnedIndexPath, error) {
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		return nil, errors.New("message index path must be absolute")
	}
	if filepath.Base(cleaned) != indexFilename {
		return nil, fmt.Errorf("message index filename must be %s", indexFilename)
	}
	parent := filepath.Dir(cleaned)
	if filepath.Base(parent) != accountID {
		return nil, errors.New("message index parent is not bound to the account ID")
	}
	directory, dirID, err := openDirectoryWithoutSymlinks(parent)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("registered account directory is missing; refusing to create an empty message index")
		}
		return nil, fmt.Errorf("open registered account directory without symlinks: %w", err)
	}
	result := &pinnedIndexPath{directory: directory, dirID: dirID}
	defer func() {
		if err != nil {
			_ = result.close()
		}
	}()

	if err = validatePrivateDirectory(directory); err != nil {
		return nil, err
	}
	if err = rejectUnsafeSQLiteSidecars(directory); err != nil {
		return nil, err
	}
	result.file, result.created, err = openIndexFile(directory, createIfMissing)
	if err != nil {
		return nil, err
	}
	result.fileID, err = validatePrivateRegularFile(result.file)
	if err != nil {
		return nil, err
	}
	if err = unix.Fchmod(int(result.file.Fd()), 0o600); err != nil {
		return nil, fmt.Errorf("protect message index: %w", err)
	}
	if result.fileID, err = validatePrivateRegularFile(result.file); err != nil {
		return nil, err
	}
	// All connections use the same canonical pathname so SQLite coordinates
	// WAL locking across repeated Store opens. The descriptor/inode proof in
	// Open detects any path exchange before the dedicated connection is kept.
	result.dsn = cleaned
	return result, nil
}

func openDirectoryWithoutSymlinks(path string) (*os.File, fileIdentity, error) {
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		return nil, fileIdentity{}, errors.New("directory path must be absolute")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	current := os.NewFile(uintptr(fd), "/")
	if current == nil {
		_ = unix.Close(fd)
		return nil, fileIdentity{}, errors.New("open root directory returned an invalid descriptor")
	}
	for _, component := range strings.Split(strings.TrimPrefix(cleaned, "/"), "/") {
		if component == "" {
			continue
		}
		nextFD, openErr := unix.Openat(
			int(current.Fd()), component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC,
			0,
		)
		if openErr != nil {
			_ = current.Close()
			return nil, fileIdentity{}, openErr
		}
		next := os.NewFile(uintptr(nextFD), component)
		if next == nil {
			_ = unix.Close(nextFD)
			_ = current.Close()
			return nil, fileIdentity{}, errors.New("open directory component returned an invalid descriptor")
		}
		if closeErr := current.Close(); closeErr != nil {
			_ = next.Close()
			return nil, fileIdentity{}, closeErr
		}
		current = next
	}
	identity, err := descriptorIdentity(current)
	if err != nil {
		_ = current.Close()
		return nil, fileIdentity{}, err
	}
	return current, identity, nil
}

func validatePrivateDirectory(directory *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("message index parent must be a directory owned by the daemon user")
	}
	if stat.Mode&0o077 != 0 {
		return errors.New("message index parent must not be accessible by other users")
	}
	return nil
}

func openIndexFile(directory *os.File, createIfMissing bool) (*os.File, bool, error) {
	flags := unix.O_RDWR | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(int(directory.Fd()), indexFilename, flags, 0)
	created := false
	if errors.Is(err, unix.ENOENT) && createIfMissing {
		fd, err = unix.Openat(int(directory.Fd()), indexFilename, flags|unix.O_CREAT|unix.O_EXCL, 0o600)
		created = err == nil
		if errors.Is(err, unix.EEXIST) {
			fd, err = unix.Openat(int(directory.Fd()), indexFilename, flags, 0)
			created = false
		}
	}
	if err != nil {
		if errors.Is(err, unix.ENOENT) && !createIfMissing {
			return nil, false, errors.New("legacy message index is missing; refusing to create it")
		}
		return nil, false, fmt.Errorf("open message index without following symlinks: %w", err)
	}
	file := os.NewFile(uintptr(fd), indexFilename)
	if file == nil {
		_ = unix.Close(fd)
		return nil, false, errors.New("open message index returned an invalid descriptor")
	}
	return file, created, nil
}

func validatePrivateRegularFile(file *os.File) (fileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fileIdentity{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fileIdentity{}, errors.New("message index must be a regular, non-symlink file")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fileIdentity{}, errors.New("message index must be owned by the daemon user")
	}
	if stat.Nlink != 1 {
		return fileIdentity{}, errors.New("message index must not have multiple hard links")
	}
	if stat.Mode&0o077 != 0 {
		return fileIdentity{}, errors.New("message index must not be accessible by other users")
	}
	return fileIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func descriptorIdentity(file *os.File) (fileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fileIdentity{}, err
	}
	return fileIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func rejectUnsafeSQLiteSidecars(directory *os.File) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		name := indexFilename + suffix
		var stat unix.Stat_t
		err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect SQLite sidecar %s: %w", name, err)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || stat.Mode&0o077 != 0 {
			return fmt.Errorf("SQLite sidecar %s must be a private regular file without links", name)
		}
	}
	return nil
}

func snapshotRegularDescriptors() (map[int]fileIdentity, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil, fmt.Errorf("inspect process descriptors: %w", err)
	}
	result := make(map[int]fileIdentity, len(entries))
	for _, entry := range entries {
		fd, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil {
			continue
		}
		var stat unix.Stat_t
		if statErr := unix.Fstat(fd, &stat); statErr != nil {
			// Descriptors owned by unrelated goroutines may disappear while the
			// procfs directory is being enumerated. They cannot prove our claim.
			continue
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		result[fd] = fileIdentity{device: uint64(stat.Dev), inode: stat.Ino}
	}
	return result, nil
}

// verifySQLiteHasPinnedInode must run while indexOpenMu is held. SQLite's
// POSIX VFS can share an existing inode descriptor between concurrent
// connections, so requiring a newly numbered descriptor is incorrect. We
// instead require a matching process descriptor that is not one of the
// anchor descriptors retained solely by this package.
func verifySQLiteHasPinnedInode(expected fileIdentity, anchors map[int]fileIdentity) error {
	descriptors, err := snapshotRegularDescriptors()
	if err != nil {
		return err
	}
	for fd, identity := range descriptors {
		if identity != expected {
			continue
		}
		if anchorIdentity, isAnchor := anchors[fd]; !isAnchor || anchorIdentity != identity {
			return nil
		}
	}
	return errors.New("SQLite has no non-anchor descriptor for the pinned message index inode")
}

func (p *pinnedIndexPath) verify(path string) error {
	if p == nil || p.directory == nil || p.file == nil {
		return errors.New("message index path is not pinned")
	}
	anchorID, err := validatePrivateRegularFile(p.file)
	if err != nil {
		return err
	}
	if anchorID != p.fileID {
		return errors.New("message index anchor inode changed")
	}
	if err := validatePrivateDirectory(p.directory); err != nil {
		return err
	}
	currentFD, err := unix.Openat(
		int(p.directory.Fd()), indexFilename,
		unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return fmt.Errorf("reopen pinned message index: %w", err)
	}
	current := os.NewFile(uintptr(currentFD), indexFilename)
	if current == nil {
		_ = unix.Close(currentFD)
		return errors.New("reopen pinned message index returned an invalid descriptor")
	}
	currentID, currentErr := validatePrivateRegularFile(current)
	closeErr := current.Close()
	if currentErr != nil {
		return currentErr
	}
	if closeErr != nil {
		return closeErr
	}
	if currentID != p.fileID {
		return errors.New("message index inode changed while opening")
	}

	canonical, canonicalID, err := openDirectoryWithoutSymlinks(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("reopen registered account directory: %w", err)
	}
	defer canonical.Close()
	if err := validatePrivateDirectory(canonical); err != nil {
		return err
	}
	if canonicalID != p.dirID {
		return errors.New("registered account directory changed while opening the message index")
	}
	return rejectUnsafeSQLiteSidecars(p.directory)
}
