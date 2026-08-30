package wecom

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	core "github.com/gih10012/wechatcopilot/internal/driver"
	"golang.org/x/sys/unix"
)

const (
	legacyWeComProfileSchemaVersion = 1
	weComProfileSchemaVersion       = 2
	weComProfileSentinelSchema      = 1
	weComProfileMetadataName        = "wecom-profile.json"
	weComProfileSentinelName        = ".wechatcopilot-profile.json"
	maxProfileDocumentBytes         = 16 << 10
)

var profileIdentityPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type profileDirectoryAnchor struct {
	file *os.File
	info os.FileInfo
}

func (anchor *profileDirectoryAnchor) close() error {
	if anchor == nil || anchor.file == nil {
		return nil
	}
	err := anchor.file.Close()
	anchor.file = nil
	return err
}

// openProfileDirectoryWithoutSymlinks resolves an absolute directory one
// component at a time from /. O_PATH keeps the resolved inode pinned without
// requiring read permission on Android's /data root, while O_NOFOLLOW plus an
// fstat check rejects a symlink in any component rather than only at the leaf.
func openProfileDirectoryWithoutSymlinks(path string) (*profileDirectoryAnchor, error) {
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		return nil, errors.New("profile directory path must be absolute")
	}
	fd, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(fd), "/")
	if current == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open profile root returned an invalid descriptor")
	}
	for _, component := range strings.Split(strings.TrimPrefix(cleaned, "/"), "/") {
		if component == "" {
			continue
		}
		nextFD, openErr := unix.Openat(
			int(current.Fd()), component,
			unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC,
			0,
		)
		if openErr != nil {
			_ = current.Close()
			return nil, openErr
		}
		var stat unix.Stat_t
		if statErr := unix.Fstat(nextFD, &stat); statErr != nil {
			_ = unix.Close(nextFD)
			_ = current.Close()
			return nil, statErr
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			_ = unix.Close(nextFD)
			_ = current.Close()
			return nil, fmt.Errorf("profile path must be a real directory, not a symlink (unsafe component %q)", component)
		}
		next := os.NewFile(uintptr(nextFD), component)
		if next == nil {
			_ = unix.Close(nextFD)
			_ = current.Close()
			return nil, errors.New("open profile directory component returned an invalid descriptor")
		}
		if closeErr := current.Close(); closeErr != nil {
			_ = next.Close()
			return nil, closeErr
		}
		current = next
	}
	info, err := current.Stat()
	if err != nil {
		_ = current.Close()
		return nil, err
	}
	return &profileDirectoryAnchor{file: current, info: info}, nil
}

func validatePrivateManagedDirectory(anchor *profileDirectoryAnchor, description string) error {
	if anchor == nil || anchor.file == nil || anchor.info == nil || !anchor.info.IsDir() {
		return fmt.Errorf("%s must be a real directory", description)
	}
	stat, ok := anchor.info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%s must be owned by the daemon user", description)
	}
	if anchor.info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must not be accessible by other users", description)
	}
	return nil
}

// weComProfileMetadata binds one persistent Android data directory to exactly
// one saved account. DataDevice is retained only to decode schema 1 records:
// st_dev is deliberately not part of the durable identity because a dm-crypt
// mapper can receive a different dev_t after a legitimate remount.
type weComProfileMetadata struct {
	SchemaVersion int       `json:"schema_version"`
	AccountID     string    `json:"account_id"`
	ProfileID     string    `json:"profile_id"`
	DataPath      string    `json:"data_path,omitempty"`
	DataDevice    uint64    `json:"data_device,omitempty"`
	DataInode     uint64    `json:"data_inode"`
	CreatedAt     time.Time `json:"created_at"`
}

// weComProfileSentinel lives inside the Android /data bind mount. Matching a
// random identity in this sentinel to the external metadata detects an
// in-place-cleared directory, which an inode-only check cannot detect.
type weComProfileSentinel struct {
	SchemaVersion int       `json:"schema_version"`
	AccountID     string    `json:"account_id"`
	ProfileID     string    `json:"profile_id"`
	CreatedAt     time.Time `json:"created_at"`
}

func validateAccountStateDir(stateDir, accountID string) (string, error) {
	if err := validateAccountID(accountID); err != nil {
		return "", err
	}
	if !filepath.IsAbs(stateDir) {
		return "", errors.New("account state directory must be absolute")
	}
	stateDir = filepath.Clean(stateDir)
	if filepath.Base(stateDir) != accountID {
		return "", errors.New("account state directory is not bound to the account ID")
	}
	anchor, err := openProfileDirectoryWithoutSymlinks(stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", errors.New("registered account state directory is missing")
	}
	if err != nil {
		return "", fmt.Errorf("inspect account state directory: %w", err)
	}
	defer anchor.close()
	if err := validatePrivateManagedDirectory(anchor, "account state directory"); err != nil {
		return "", err
	}
	return stateDir, nil
}

func ensureManagedDirectory(path string) error {
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) || filepath.Base(cleaned) == "." || filepath.Base(cleaned) == string(filepath.Separator) {
		return errors.New("managed directory path must name an absolute child directory")
	}
	parent, err := openProfileDirectoryWithoutSymlinks(filepath.Dir(cleaned))
	if err != nil {
		return err
	}
	defer parent.close()
	name := filepath.Base(cleaned)
	child, err := openProfileChildDirectory(parent, name)
	if errors.Is(err, fs.ErrNotExist) {
		if err := unix.Mkdirat(int(parent.file.Fd()), name, 0o700); err != nil {
			return err
		}
		if err := syncProfileDirectory(parent); err != nil {
			return err
		}
		child, err = openProfileChildDirectory(parent, name)
	}
	if err != nil {
		return err
	}
	defer child.close()
	return makePrivateManagedDirectory(child, "managed directory")
}

func inspectRealDirectory(path string) (os.FileInfo, bool, error) {
	anchor, err := openProfileDirectoryWithoutSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer anchor.close()
	return anchor.info, true, nil
}

func openProfileChildDirectory(parent *profileDirectoryAnchor, name string) (*profileDirectoryAnchor, error) {
	if parent == nil || parent.file == nil || name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return nil, errors.New("profile child directory name is invalid")
	}
	fd, err := unix.Openat(
		int(parent.file.Fd()), name,
		unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(fd)
		return nil, errors.New("profile child path must be a real directory, not a symlink")
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open profile child directory returned an invalid descriptor")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &profileDirectoryAnchor{file: file, info: info}, nil
}

func createManagedDirectory(path string) (os.FileInfo, error) {
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) || filepath.Base(cleaned) == "." || filepath.Base(cleaned) == string(filepath.Separator) {
		return nil, errors.New("managed directory path must name an absolute child directory")
	}
	parent, err := openProfileDirectoryWithoutSymlinks(filepath.Dir(cleaned))
	if err != nil {
		return nil, err
	}
	defer parent.close()
	name := filepath.Base(cleaned)
	if err := unix.Mkdirat(int(parent.file.Fd()), name, 0o700); err != nil {
		return nil, err
	}
	if err := syncProfileDirectory(parent); err != nil {
		return nil, err
	}
	child, err := openProfileChildDirectory(parent, name)
	if err != nil {
		return nil, err
	}
	defer child.close()
	if err := validatePrivateManagedDirectory(child, "managed directory"); err != nil {
		return nil, err
	}
	return child.info, nil
}

func makePrivateManagedDirectory(anchor *profileDirectoryAnchor, description string) error {
	if anchor == nil || anchor.file == nil || anchor.info == nil {
		return fmt.Errorf("%s is not pinned", description)
	}
	stat, ok := anchor.info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%s must be owned by the daemon user", description)
	}
	fd, err := unix.Openat(
		int(anchor.file.Fd()), ".",
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return err
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		_ = unix.Close(fd)
		return err
	}
	if err := unix.Close(fd); err != nil {
		return err
	}
	info, err := anchor.file.Stat()
	if err != nil {
		return err
	}
	anchor.info = info
	return validatePrivateManagedDirectory(anchor, description)
}

func syncProfileDirectory(anchor *profileDirectoryAnchor) error {
	if anchor == nil || anchor.file == nil {
		return errors.New("profile directory is not pinned")
	}
	fd, err := unix.Openat(
		int(anchor.file.Fd()), ".",
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), ".")
	if directory == nil {
		_ = unix.Close(fd)
		return errors.New("reopen profile directory returned an invalid descriptor")
	}
	defer directory.Close()
	return directory.Sync()
}

func profileMetadataPath(stateDir string) string {
	return filepath.Join(stateDir, weComProfileMetadataName)
}

func profileSentinelPath(dataDir string) string {
	return filepath.Join(dataDir, weComProfileSentinelName)
}

func readWeComProfileMetadata(path string) (weComProfileMetadata, bool, error) {
	var metadata weComProfileMetadata
	exists, err := readPrivateProfileDocument(path, "WeCom profile metadata", &metadata)
	if err != nil {
		return weComProfileMetadata{}, exists, err
	}
	return metadata, exists, nil
}

func readWeComProfileSentinel(path string) (weComProfileSentinel, bool, error) {
	var sentinel weComProfileSentinel
	exists, err := readPrivateProfileDocument(path, "WeCom internal profile sentinel", &sentinel)
	if err != nil {
		return weComProfileSentinel{}, exists, err
	}
	return sentinel, exists, nil
}

func readPrivateProfileDocument(path, description string, destination any) (exists bool, resultErr error) {
	cleaned := filepath.Clean(path)
	directory, err := openProfileDirectoryWithoutSymlinks(filepath.Dir(cleaned))
	if errors.Is(err, syscall.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open parent of %s without following symlinks: %w", description, err)
	}
	defer directory.close()
	return readPrivateProfileDocumentAt(directory, filepath.Base(cleaned), description, destination)
}

// readPrivateProfileDocumentAt reads through an already pinned directory.
// Security-sensitive profile validation must not resolve the data pathname a
// second time, because an A -> B -> A exchange could otherwise supply a
// sentinel from a different directory between two inode checks.
func readPrivateProfileDocumentAt(
	directory *profileDirectoryAnchor,
	name, description string,
	destination any,
) (exists bool, resultErr error) {
	handle, exists, err := openPrivateProfileDocumentAt(directory, name, description, destination)
	if err != nil || !exists {
		return exists, err
	}
	return true, handle.close()
}

type privateProfileDocumentHandle struct {
	file *os.File
	stat unix.Stat_t
}

func (handle *privateProfileDocumentHandle) close() error {
	if handle == nil || handle.file == nil {
		return nil
	}
	err := handle.file.Close()
	handle.file = nil
	return err
}

func openPrivateProfileDocumentAt(
	directory *profileDirectoryAnchor,
	name, description string,
	destination any,
) (*privateProfileDocumentHandle, bool, error) {
	if directory == nil || directory.file == nil {
		return nil, false, fmt.Errorf("parent of %s is not pinned", description)
	}
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return nil, false, fmt.Errorf("%s path is invalid", description)
	}
	fd, err := unix.Openat(
		int(directory.file.Fd()), name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if errors.Is(err, unix.ENOENT) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("open %s without following symlinks: %w", description, err)
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, true, fmt.Errorf("open %s returned an invalid descriptor", description)
	}
	handle := &privateProfileDocumentHandle{file: file}
	fail := func(err error) (*privateProfileDocumentHandle, bool, error) {
		return nil, true, errors.Join(err, handle.close())
	}
	if err := unix.Fstat(fd, &handle.stat); err != nil {
		return fail(err)
	}
	if handle.stat.Mode&unix.S_IFMT != unix.S_IFREG || handle.stat.Uid != uint32(os.Geteuid()) || handle.stat.Nlink != 1 {
		return fail(fmt.Errorf("%s must be a regular file owned by the daemon user without links", description))
	}
	if handle.stat.Mode&0o077 != 0 {
		return fail(fmt.Errorf("%s is accessible by other users", description))
	}
	if handle.stat.Size <= 0 || handle.stat.Size > maxProfileDocumentBytes {
		return fail(fmt.Errorf("%s has an invalid size", description))
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxProfileDocumentBytes+1))
	if err != nil {
		return fail(err)
	}
	if err := decodeProfileDocument(contents, description, destination); err != nil {
		return fail(err)
	}
	return handle, true, nil
}

func decodeProfileDocument(contents []byte, description string, destination any) error {
	if len(contents) == 0 || len(contents) > maxProfileDocumentBytes {
		return fmt.Errorf("%s has an invalid size", description)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode %s: %w", description, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode %s: trailing JSON value", description)
		}
		return fmt.Errorf("decode %s: %w", description, err)
	}
	return nil
}

func validateWeComProfileMetadata(metadata weComProfileMetadata, accountID string, dataInfo os.FileInfo) error {
	if metadata.SchemaVersion != legacyWeComProfileSchemaVersion && metadata.SchemaVersion != weComProfileSchemaVersion {
		return fmt.Errorf("unsupported WeCom profile metadata schema %d", metadata.SchemaVersion)
	}
	if metadata.AccountID != accountID {
		return errors.New("WeCom profile metadata belongs to another account")
	}
	if !profileIdentityPattern.MatchString(metadata.ProfileID) || metadata.CreatedAt.IsZero() {
		return errors.New("WeCom profile metadata has an invalid stable identity")
	}
	_, inode, err := directoryIdentity(dataInfo)
	if err != nil {
		return err
	}
	if metadata.DataInode == 0 || metadata.DataInode != inode {
		return errors.New("WeCom Android data directory was replaced or exchanged")
	}
	switch metadata.SchemaVersion {
	case legacyWeComProfileSchemaVersion:
		if metadata.DataDevice == 0 || metadata.DataPath != "" {
			return errors.New("legacy WeCom profile metadata has invalid identity fields")
		}
	case weComProfileSchemaVersion:
		if metadata.DataDevice != 0 || !canonicalAbsoluteProfilePath(metadata.DataPath) {
			return errors.New("WeCom profile metadata has an invalid canonical data path")
		}
	}
	return nil
}

func validateWeComProfileMetadataPath(metadata weComProfileMetadata, dataDir string) error {
	if metadata.SchemaVersion == legacyWeComProfileSchemaVersion {
		return nil
	}
	if !canonicalAbsoluteProfilePath(dataDir) || metadata.DataPath != filepath.Clean(dataDir) {
		return errors.New("WeCom profile metadata is bound to another canonical data path")
	}
	return nil
}

func canonicalAbsoluteProfilePath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func validateWeComProfileSentinel(sentinel weComProfileSentinel, metadata weComProfileMetadata) error {
	if sentinel.SchemaVersion != weComProfileSentinelSchema {
		return fmt.Errorf("unsupported WeCom internal profile sentinel schema %d", sentinel.SchemaVersion)
	}
	if sentinel.AccountID != metadata.AccountID {
		return errors.New("WeCom internal profile sentinel belongs to another account")
	}
	if !profileIdentityPattern.MatchString(sentinel.ProfileID) || sentinel.ProfileID != metadata.ProfileID {
		return errors.New("WeCom internal profile sentinel identity does not match external metadata")
	}
	if sentinel.CreatedAt.IsZero() || !sentinel.CreatedAt.Equal(metadata.CreatedAt) {
		return errors.New("WeCom internal profile sentinel creation identity does not match external metadata")
	}
	return nil
}

func validateStandaloneProfileSentinel(sentinel weComProfileSentinel, accountID string) error {
	metadata := weComProfileMetadata{
		AccountID: accountID,
		ProfileID: sentinel.ProfileID,
		CreatedAt: sentinel.CreatedAt,
	}
	return validateWeComProfileSentinel(sentinel, metadata)
}

func sentinelForMetadata(metadata weComProfileMetadata) weComProfileSentinel {
	return weComProfileSentinel{
		SchemaVersion: weComProfileSentinelSchema,
		AccountID:     metadata.AccountID,
		ProfileID:     metadata.ProfileID,
		CreatedAt:     metadata.CreatedAt,
	}
}

func metadataForSentinel(sentinel weComProfileSentinel, dataDir string, dataInfo os.FileInfo) (weComProfileMetadata, error) {
	_, inode, err := directoryIdentity(dataInfo)
	if err != nil {
		return weComProfileMetadata{}, err
	}
	return weComProfileMetadata{
		SchemaVersion: weComProfileSchemaVersion,
		AccountID:     sentinel.AccountID,
		ProfileID:     sentinel.ProfileID,
		DataPath:      filepath.Clean(dataDir),
		DataInode:     inode,
		CreatedAt:     sentinel.CreatedAt,
	}, nil
}

func newWeComProfileMetadata(accountID, dataDir string, dataInfo os.FileInfo) (weComProfileMetadata, error) {
	_, inode, err := directoryIdentity(dataInfo)
	if err != nil {
		return weComProfileMetadata{}, err
	}
	var identity [16]byte
	if _, err := rand.Read(identity[:]); err != nil {
		return weComProfileMetadata{}, fmt.Errorf("generate stable WeCom profile identity: %w", err)
	}
	return weComProfileMetadata{
		SchemaVersion: weComProfileSchemaVersion,
		AccountID:     accountID,
		ProfileID:     hex.EncodeToString(identity[:]),
		DataPath:      filepath.Clean(dataDir),
		DataInode:     inode,
		CreatedAt:     time.Now().UTC(),
	}, nil
}

func verifyAndMigrateWeComProfileMetadata(
	accountID, dataDir, metadataPath string,
	metadata weComProfileMetadata,
) (weComProfileMetadata, error) {
	if err := verifyBoundWeComProfile(accountID, dataDir, metadata); err != nil {
		return weComProfileMetadata{}, err
	}
	if metadata.SchemaVersion == weComProfileSchemaVersion {
		return metadata, nil
	}
	return migrateLegacyWeComProfileMetadata(accountID, dataDir, metadataPath, metadata)
}

// migrateLegacyWeComProfileMetadata removes schema 1's transient st_dev from
// the durable identity. The old file remains recoverable until an atomic
// exchange has installed and validated schema 2 against the same pinned data
// inode and sentinel.
func migrateLegacyWeComProfileMetadata(
	accountID, dataDir, metadataPath string,
	legacy weComProfileMetadata,
) (result weComProfileMetadata, resultErr error) {
	if legacy.SchemaVersion != legacyWeComProfileSchemaVersion {
		return weComProfileMetadata{}, fmt.Errorf("unsupported WeCom profile metadata schema %d", legacy.SchemaVersion)
	}
	data, err := openProfileDirectoryWithoutSymlinks(dataDir)
	if err != nil {
		return weComProfileMetadata{}, fmt.Errorf("pin legacy WeCom data for metadata migration: %w", err)
	}
	defer data.close()
	if err := verifyBoundWeComProfileAt(accountID, dataDir, legacy, data); err != nil {
		return weComProfileMetadata{}, fmt.Errorf("verify legacy WeCom profile before metadata migration: %w", err)
	}

	current := legacy
	current.SchemaVersion = weComProfileSchemaVersion
	current.DataPath = filepath.Clean(dataDir)
	current.DataDevice = 0
	if err := validateWeComProfileMetadata(current, accountID, data.info); err != nil {
		return weComProfileMetadata{}, err
	}
	if err := validateWeComProfileMetadataPath(current, dataDir); err != nil {
		return weComProfileMetadata{}, err
	}

	stateDirectory, err := openProfileDirectoryWithoutSymlinks(filepath.Dir(filepath.Clean(metadataPath)))
	if err != nil {
		return weComProfileMetadata{}, fmt.Errorf("pin account state directory for metadata migration: %w", err)
	}
	defer stateDirectory.close()
	if err := validatePrivateManagedDirectory(stateDirectory, "account state directory"); err != nil {
		return weComProfileMetadata{}, err
	}
	var pinnedLegacy weComProfileMetadata
	legacyHandle, exists, err := openPrivateProfileDocumentAt(
		stateDirectory, filepath.Base(metadataPath), "legacy WeCom profile metadata", &pinnedLegacy,
	)
	if err != nil {
		return weComProfileMetadata{}, err
	}
	if !exists {
		return weComProfileMetadata{}, errors.New("legacy WeCom profile metadata disappeared during migration")
	}
	defer func() { resultErr = errors.Join(resultErr, legacyHandle.close()) }()
	if !sameWeComProfileMetadata(pinnedLegacy, legacy) {
		return weComProfileMetadata{}, errors.New("legacy WeCom profile metadata changed before migration")
	}

	candidateName, err := newProfileTemporaryName(".wecom-profile-migration-*.new")
	if err != nil {
		return weComProfileMetadata{}, err
	}
	candidate, err := writeNewProfileDocumentAt(
		stateDirectory, candidateName, current,
		".wecom-profile-migration-*.tmp", "migrated WeCom profile metadata",
	)
	if err != nil {
		if candidate != nil {
			err = errors.Join(err, candidate.remove())
		}
		return weComProfileMetadata{}, err
	}
	candidateIsNew := true
	defer func() {
		if candidateIsNew {
			resultErr = errors.Join(resultErr, candidate.remove())
		}
	}()

	if err := verifyBoundWeComProfileAt(accountID, dataDir, legacy, data); err != nil {
		return weComProfileMetadata{}, fmt.Errorf("legacy WeCom profile changed during metadata migration: %w", err)
	}
	var currentLegacyStat unix.Stat_t
	if err := unix.Fstatat(
		int(stateDirectory.file.Fd()), filepath.Base(metadataPath), &currentLegacyStat, unix.AT_SYMLINK_NOFOLLOW,
	); err != nil || !samePublishedProfileDocument(legacyHandle.stat, currentLegacyStat) {
		return weComProfileMetadata{}, errors.New("legacy WeCom profile metadata changed during migration")
	}
	if err := unix.Renameat2(
		int(stateDirectory.file.Fd()), candidateName,
		int(stateDirectory.file.Fd()), filepath.Base(metadataPath),
		unix.RENAME_EXCHANGE,
	); err != nil {
		return weComProfileMetadata{}, fmt.Errorf("atomically migrate WeCom profile metadata: %w", err)
	}
	candidateIsNew = false
	rollback := func(cause error) error {
		if err := rollbackWeComProfileMetadataExchange(
			stateDirectory, filepath.Base(metadataPath), candidateName, legacyHandle.stat, candidate.stat,
		); err != nil {
			return errors.Join(cause, fmt.Errorf("preserve legacy WeCom metadata after failed migration: %w", err))
		}
		candidateIsNew = true
		return cause
	}

	var installedStat, displacedStat unix.Stat_t
	if err := unix.Fstatat(int(stateDirectory.file.Fd()), filepath.Base(metadataPath), &installedStat, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		!samePublishedProfileDocument(candidate.stat, installedStat) {
		return weComProfileMetadata{}, rollback(errors.New("migrated WeCom profile metadata changed during atomic exchange"))
	}
	if err := unix.Fstatat(int(stateDirectory.file.Fd()), candidateName, &displacedStat, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		!samePublishedProfileDocument(legacyHandle.stat, displacedStat) {
		return weComProfileMetadata{}, rollback(errors.New("legacy WeCom profile metadata changed during atomic exchange"))
	}
	if err := verifyBoundWeComProfileAt(accountID, dataDir, current, data); err != nil {
		return weComProfileMetadata{}, rollback(fmt.Errorf("verify migrated WeCom profile metadata: %w", err))
	}
	if err := syncProfileDirectory(stateDirectory); err != nil {
		return weComProfileMetadata{}, rollback(fmt.Errorf("persist migrated WeCom profile metadata: %w", err))
	}
	if err := unix.Fstatat(int(stateDirectory.file.Fd()), candidateName, &displacedStat, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		!samePublishedProfileDocument(legacyHandle.stat, displacedStat) {
		return weComProfileMetadata{}, errors.New("displaced legacy WeCom profile metadata changed before cleanup")
	}
	if err := unix.Unlinkat(int(stateDirectory.file.Fd()), candidateName, 0); err != nil {
		return weComProfileMetadata{}, fmt.Errorf("remove displaced legacy WeCom profile metadata: %w", err)
	}
	if err := syncProfileDirectory(stateDirectory); err != nil {
		return weComProfileMetadata{}, fmt.Errorf("persist legacy WeCom metadata cleanup: %w", err)
	}
	return current, nil
}

func rollbackWeComProfileMetadataExchange(
	directory *profileDirectoryAnchor,
	targetName, candidateName string,
	legacyStat, currentStat unix.Stat_t,
) error {
	var target, candidate unix.Stat_t
	if err := unix.Fstatat(int(directory.file.Fd()), targetName, &target, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if err := unix.Fstatat(int(directory.file.Fd()), candidateName, &candidate, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if !samePublishedProfileDocument(currentStat, target) || !samePublishedProfileDocument(legacyStat, candidate) {
		return errors.New("metadata exchange entries no longer match the pinned migration files")
	}
	if err := unix.Renameat2(
		int(directory.file.Fd()), candidateName,
		int(directory.file.Fd()), targetName,
		unix.RENAME_EXCHANGE,
	); err != nil {
		return err
	}
	if err := syncProfileDirectory(directory); err != nil {
		return err
	}
	if err := unix.Fstatat(int(directory.file.Fd()), targetName, &target, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if err := unix.Fstatat(int(directory.file.Fd()), candidateName, &candidate, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if !samePublishedProfileDocument(legacyStat, target) || !samePublishedProfileDocument(currentStat, candidate) {
		return errors.New("metadata exchange rollback did not restore the pinned files")
	}
	return nil
}

func sameWeComProfileMetadata(left, right weComProfileMetadata) bool {
	return left.SchemaVersion == right.SchemaVersion && left.AccountID == right.AccountID &&
		left.ProfileID == right.ProfileID && left.DataPath == right.DataPath &&
		left.DataDevice == right.DataDevice && left.DataInode == right.DataInode &&
		left.CreatedAt.Equal(right.CreatedAt)
}

func directoryIdentity(info os.FileInfo) (uint64, uint64, error) {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return 0, 0, errors.New("WeCom Android data path is not a real directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 {
		return 0, 0, errors.New("cannot determine WeCom Android data directory identity")
	}
	return uint64(stat.Dev), stat.Ino, nil
}

// writeNewWeComProfileMetadata publishes a new marker without replacing any
// existing marker. renameat2 avoids the two-link crash window of a temporary
// hard-link publication while retaining atomic no-replace semantics.
func writeNewWeComProfileMetadata(path string, metadata weComProfileMetadata) (resultErr error) {
	return writeNewProfileDocument(path, metadata, ".wecom-profile-*.tmp", "WeCom profile metadata")
}

func writeNewWeComProfileSentinel(path string, sentinel weComProfileSentinel) error {
	return writeNewProfileDocument(path, sentinel, ".wechatcopilot-profile-*.tmp", "WeCom internal profile sentinel")
}

func writeNewProfileDocument(path string, value any, temporaryPattern, description string) (resultErr error) {
	cleaned := filepath.Clean(path)
	directory, err := openProfileDirectoryWithoutSymlinks(filepath.Dir(cleaned))
	if err != nil {
		return err
	}
	defer directory.close()
	publication, err := writeNewProfileDocumentAt(
		directory, filepath.Base(cleaned), value, temporaryPattern, description,
	)
	if err != nil && publication != nil {
		err = errors.Join(err, publication.remove())
	}
	return err
}

type profileDocumentPublication struct {
	directory *profileDirectoryAnchor
	name      string
	stat      unix.Stat_t
}

func writeNewProfileDocumentAt(
	directory *profileDirectoryAnchor,
	targetName string,
	value any,
	temporaryPattern, description string,
) (publication *profileDocumentPublication, resultErr error) {
	if directory == nil || directory.file == nil {
		return nil, fmt.Errorf("parent of %s is not pinned", description)
	}
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	contents = append(contents, '\n')
	if targetName == "" || targetName == "." || targetName == ".." || strings.ContainsRune(targetName, filepath.Separator) {
		return nil, fmt.Errorf("%s path is invalid", description)
	}
	temporaryName, err := newProfileTemporaryName(temporaryPattern)
	if err != nil {
		return nil, err
	}
	temporaryFD, err := unix.Openat(
		int(directory.file.Fd()), temporaryName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return nil, err
	}
	temporary := os.NewFile(uintptr(temporaryFD), temporaryName)
	if temporary == nil {
		_ = unix.Close(temporaryFD)
		return nil, errors.New("create profile temporary file returned an invalid descriptor")
	}
	closed := false
	defer func() {
		if !closed {
			closeErr := temporary.Close()
			resultErr = errors.Join(resultErr, closeErr)
		}
		if removeErr := unix.Unlinkat(int(directory.file.Fd()), temporaryName, 0); removeErr != nil && !errors.Is(removeErr, unix.ENOENT) {
			resultErr = errors.Join(resultErr, removeErr)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return nil, err
	}
	if _, err := temporary.Write(contents); err != nil {
		return nil, err
	}
	if err := temporary.Sync(); err != nil {
		return nil, err
	}
	var temporaryStat unix.Stat_t
	if err := unix.Fstat(temporaryFD, &temporaryStat); err != nil {
		return nil, err
	}
	if temporaryStat.Mode&unix.S_IFMT != unix.S_IFREG || temporaryStat.Uid != uint32(os.Geteuid()) ||
		temporaryStat.Nlink != 1 || temporaryStat.Mode&0o777 != 0o600 {
		return nil, fmt.Errorf("%s temporary file identity or permissions changed before publication", description)
	}
	closeErr := temporary.Close()
	closed = true
	if closeErr != nil {
		return nil, closeErr
	}
	if err := unix.Renameat2(
		int(directory.file.Fd()), temporaryName,
		int(directory.file.Fd()), targetName,
		unix.RENAME_NOREPLACE,
	); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("%s already exists", description)
		}
		return nil, err
	}
	publication = &profileDocumentPublication{directory: directory, name: targetName, stat: temporaryStat}
	if err := syncProfileDirectory(directory); err != nil {
		return publication, err
	}
	var current unix.Stat_t
	if err := unix.Fstatat(int(directory.file.Fd()), targetName, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return publication, err
	}
	if !samePublishedProfileDocument(temporaryStat, current) {
		return publication, fmt.Errorf("%s changed during publication", description)
	}
	return publication, nil
}

func (publication *profileDocumentPublication) remove() error {
	if publication == nil || publication.directory == nil || publication.directory.file == nil {
		return nil
	}
	var current unix.Stat_t
	if err := unix.Fstatat(
		int(publication.directory.file.Fd()), publication.name, &current, unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if !samePublishedProfileDocument(publication.stat, current) {
		return errors.New("published profile document changed before rollback")
	}
	if err := unix.Unlinkat(int(publication.directory.file.Fd()), publication.name, 0); err != nil {
		return err
	}
	return syncProfileDirectory(publication.directory)
}

func samePublishedProfileDocument(expected, actual unix.Stat_t) bool {
	return expected.Dev == actual.Dev && expected.Ino == actual.Ino && expected.Size == actual.Size &&
		actual.Mode&unix.S_IFMT == unix.S_IFREG && actual.Uid == uint32(os.Geteuid()) &&
		actual.Nlink == 1 && actual.Mode&0o777 == 0o600
}

func newProfileTemporaryName(pattern string) (string, error) {
	prefix, suffix, ok := strings.Cut(pattern, "*")
	if !ok || strings.Contains(prefix, "/") || strings.Contains(suffix, "/") {
		return "", errors.New("profile temporary filename pattern is invalid")
	}
	var identity [16]byte
	if _, err := rand.Read(identity[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(identity[:]) + suffix, nil
}

func (r *Runtime) prepareLegacyDataDirectoryForSentinel(
	ctx context.Context,
	account core.AccountRuntime,
	dataInfo os.FileInfo,
	containerRunning bool,
) (os.FileInfo, error) {
	stat, ok := dataInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("cannot inspect legacy WeCom Android data ownership")
	}
	if stat.Uid == uint32(os.Geteuid()) {
		return dataInfo, nil
	}
	expectedDevice, expectedInode, err := directoryIdentity(dataInfo)
	if err != nil {
		return nil, err
	}
	if containerRunning {
		runningContainerID, err := r.inspectExactRunningContainer(ctx, account.AccountID, r.dataDir)
		if err != nil {
			return nil, fmt.Errorf("revalidate live legacy WeCom container before ownership migration: %w", err)
		}
		seconds := int(r.config.StopGrace.Round(time.Second) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		if _, err := r.executor.Run(
			ctx, r.config.DockerBinary, "container", "stop", "--time", strconv.Itoa(seconds), runningContainerID,
		); err != nil {
			return nil, fmt.Errorf("stop live legacy WeCom container before ownership migration: %w", err)
		}
		stoppedContainerID, err := r.inspectExactStoppedContainer(ctx, account.AccountID, r.dataDir)
		if err != nil {
			return nil, fmt.Errorf("verify stopped legacy WeCom container before ownership migration: %w", err)
		}
		if stoppedContainerID != runningContainerID {
			return nil, errors.New("legacy WeCom container changed during ownership migration stop")
		}
	}
	if err := verifyCanonicalDataIdentity(r.dataDir, expectedDevice, expectedInode); err != nil {
		return nil, fmt.Errorf("verify legacy WeCom data before ownership migration: %w", err)
	}
	if _, err := r.executor.RunInput(
		ctx, nil, 1024,
		r.config.DockerBinary, legacyProfileOwnershipHelperArgs(
			r.config.RedroidImage, r.dataDir, os.Geteuid(), os.Getegid(),
			expectedDevice, expectedInode,
		)...,
	); err != nil {
		return nil, fmt.Errorf("run restricted legacy WeCom profile ownership helper: %w", err)
	}
	if err := verifyCanonicalDataIdentity(r.dataDir, expectedDevice, expectedInode); err != nil {
		return nil, fmt.Errorf("verify legacy WeCom data after ownership migration: %w", err)
	}
	rechecked, exists, err := inspectRealDirectory(r.dataDir)
	if err != nil || !exists {
		return nil, fmt.Errorf("inspect legacy WeCom data after ownership migration: %w", err)
	}
	recheckedStat, ok := rechecked.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("cannot inspect migrated legacy WeCom Android data ownership")
	}
	if err := validateLegacyDataOwnership(recheckedStat, uint32(os.Geteuid()), uint32(os.Getegid())); err != nil {
		return nil, errors.New("restricted legacy WeCom ownership helper did not bind /data to the daemon identity")
	}
	if err := protectDataDirectory(r.dataDir); err != nil {
		return nil, err
	}
	rechecked, exists, err = inspectRealDirectory(r.dataDir)
	if err != nil || !exists {
		return nil, fmt.Errorf("recheck protected legacy WeCom data: %w", err)
	}
	return rechecked, nil
}

func validateLegacyDataOwnership(stat *syscall.Stat_t, expectedUID, expectedGID uint32) error {
	if stat == nil || stat.Uid != expectedUID || stat.Gid != expectedGID {
		return errors.New("legacy WeCom Android data ownership does not match the daemon identity")
	}
	return nil
}

func legacyProfileOwnershipHelperArgs(
	image, dataDir string,
	uid, gid int,
	expectedDevice, expectedInode uint64,
) []string {
	const verifyAndChown = `actual=$(/system/bin/toybox stat -Lc %d:%i /data) || exit 71
[ "$actual" = "$1:$2" ] || exit 72
exec /system/bin/toybox chown "$3:$4" /data`
	return []string{
		"container", "run", "--rm", "--pull", "never",
		"--network", "none", "--read-only", "--pids-limit", "32", "--memory", "64m",
		"--cap-drop", "ALL", "--cap-add", "CHOWN",
		"--security-opt", "no-new-privileges=true", "--user", "0:0",
		"--mount", "type=bind,src=" + dataDir + ",dst=/data",
		"--entrypoint", "/system/bin/toybox",
		image, "sh", "-c", verifyAndChown, "wechatcopilot-ownership-helper",
		strconv.FormatUint(expectedDevice, 10), strconv.FormatUint(expectedInode, 10),
		strconv.Itoa(uid), strconv.Itoa(gid),
	}
}

// preparePersistentProfile distinguishes a new account from a legacy profile
// and from a damaged registered profile. Legacy adoption is allowed only when
// Docker independently proves exact account ownership and mount identity.
func (r *Runtime) preparePersistentProfile(ctx context.Context, account core.AccountRuntime) error {
	metadataPath := profileMetadataPath(account.StateDir)
	metadata, metadataExists, err := readWeComProfileMetadata(metadataPath)
	if err != nil {
		return fmt.Errorf("read WeCom profile metadata: %w", err)
	}
	dataInfo, dataExists, err := inspectRealDirectory(r.dataDir)
	if err != nil {
		return fmt.Errorf("inspect WeCom Android data directory: %w", err)
	}

	if metadataExists {
		metadata, err = verifyAndMigrateWeComProfileMetadata(
			account.AccountID, r.dataDir, metadataPath, metadata,
		)
		if err != nil {
			return err
		}
		if _, _, err := r.inspectContainer(ctx, account.AccountID, r.dataDir); err != nil {
			return err
		}
		if err := protectDataDirectory(r.dataDir); err != nil {
			return err
		}
		return verifyBoundWeComProfile(account.AccountID, r.dataDir, metadata)
	}

	exists, containerRunning, err := r.inspectContainer(ctx, account.AccountID, r.dataDir)
	if err != nil {
		return err
	}
	if dataExists {
		if !exists {
			data, err := openProfileDirectoryWithoutSymlinks(r.dataDir)
			if err != nil {
				return fmt.Errorf("pin WeCom Android data during initialization recovery: %w", err)
			}
			defer data.close()
			beforeDevice, beforeInode, err := directoryIdentity(dataInfo)
			if err != nil {
				return err
			}
			pinnedDevice, pinnedInode, err := directoryIdentity(data.info)
			if err != nil || pinnedDevice != beforeDevice || pinnedInode != beforeInode {
				return errors.New("WeCom Android data directory changed before initialization recovery")
			}
			entries, err := readPinnedProfileDirectoryAt(data)
			if err != nil {
				return fmt.Errorf("verify interrupted WeCom profile initialization: %w", err)
			}
			switch {
			case len(entries) == 0:
				if err := protectPinnedDataDirectory(r.dataDir, data); err != nil {
					return err
				}
				return initializeWeComProfileDocumentsAt(
					account.AccountID, r.dataDir, metadataPath, data, nil,
				)
			case len(entries) == 1 && entries[0].Name() == weComProfileSentinelName:
				var sentinel weComProfileSentinel
				sentinelExists, err := readPrivateProfileDocumentAt(
					data, weComProfileSentinelName,
					"interrupted WeCom internal profile sentinel", &sentinel,
				)
				if err != nil {
					return fmt.Errorf("verify interrupted WeCom profile initialization: %w", err)
				}
				if !sentinelExists {
					return errors.New("interrupted WeCom internal profile sentinel disappeared during validation")
				}
				if err := validateStandaloneProfileSentinel(sentinel, account.AccountID); err != nil {
					return fmt.Errorf("verify interrupted WeCom profile initialization: %w", err)
				}
				metadata, err = metadataForSentinel(sentinel, r.dataDir, data.info)
				if err != nil {
					return err
				}
				return publishLegacyWeComProfileBinding(
					account.AccountID, r.dataDir, metadataPath, metadata, data, false, nil,
				)
			default:
				return errors.New("unmarked WeCom Android data exists without a verifiable account container; refusing automatic adoption")
			}
		}
		networkExists, err := r.inspectNetwork(ctx, account.AccountID)
		if err != nil {
			return fmt.Errorf("verify isolated account network before legacy profile adoption: %w", err)
		}
		if !networkExists {
			return errors.New("legacy WeCom profile container has no verifiable isolated account network")
		}
		verifiedInfo, stillExists, err := inspectRealDirectory(r.dataDir)
		if err != nil || !stillExists {
			return errors.New("legacy WeCom Android data changed during adoption")
		}
		beforeDevice, beforeInode, err := directoryIdentity(dataInfo)
		if err != nil {
			return err
		}
		afterDevice, afterInode, err := directoryIdentity(verifiedInfo)
		if err != nil {
			return err
		}
		if beforeDevice != afterDevice || beforeInode != afterInode {
			return errors.New("legacy WeCom Android data changed during adoption")
		}
		verifiedData, err := openProfileDirectoryWithoutSymlinks(r.dataDir)
		if err != nil {
			return fmt.Errorf("pin legacy WeCom Android data during adoption: %w", err)
		}
		defer verifiedData.close()
		pinnedDevice, pinnedInode, err := directoryIdentity(verifiedData.info)
		if err != nil || pinnedDevice != beforeDevice || pinnedInode != beforeInode {
			return errors.New("legacy WeCom Android data changed before it could be pinned")
		}
		exists, containerRunning, err = r.inspectContainer(ctx, account.AccountID, r.dataDir)
		if err != nil {
			return fmt.Errorf("legacy WeCom account container changed during adoption: %w", err)
		}
		if !exists {
			return errors.New("legacy WeCom account container changed during adoption")
		}
		var sentinel weComProfileSentinel
		sentinelExists, err := readPrivateProfileDocumentAt(
			verifiedData, weComProfileSentinelName, "legacy WeCom internal profile sentinel", &sentinel,
		)
		if err != nil {
			return fmt.Errorf("inspect legacy WeCom internal profile sentinel: %w", err)
		}
		if sentinelExists {
			if err := validateStandaloneProfileSentinel(sentinel, account.AccountID); err != nil {
				return fmt.Errorf("verify legacy WeCom internal profile sentinel: %w", err)
			}
			metadata, err = metadataForSentinel(sentinel, r.dataDir, verifiedData.info)
			if err != nil {
				return err
			}
		}

		// An internal sentinel without its external metadata can be the result
		// of an interrupted publication, but it is not itself authorization to
		// adopt an existing container. Gate both sentinel-present and
		// sentinel-absent legacy profiles on the same live proof or explicit
		// one-use offline approval before publishing any metadata.
		if containerRunning {
			if sentinelExists {
				if _, err := r.verifyRunningContainerProfileFrame(ctx, account, metadata, true, ""); err != nil {
					return fmt.Errorf("prove live legacy WeCom profile before metadata binding: %w", err)
				}
			} else {
				if err := r.verifyRunningLegacyDataIdentity(ctx, account, verifiedData.info); err != nil {
					return fmt.Errorf("prove live legacy WeCom data before sentinel binding: %w", err)
				}
			}
			if _, err := consumeLegacyProfileApprovalIfPresent(
				account.StateDir, account.AccountID, r.dataDir,
			); err != nil {
				return fmt.Errorf("revoke superseded legacy WeCom profile approval: %w", err)
			}
		} else {
			stoppedContainerEpoch, err := r.inspectExactStoppedContainerEpoch(ctx, account.AccountID, r.dataDir)
			if err != nil {
				return fmt.Errorf("prove stopped legacy WeCom container before approval consumption: %w", err)
			}
			if err := consumeLegacyProfileApprovalForData(
				account.StateDir, account.AccountID, r.dataDir, stoppedContainerEpoch,
			); err != nil {
				return fmt.Errorf("authorize stopped legacy WeCom profile adoption: %w", err)
			}
			verifiedContainerEpoch, err := r.inspectExactStoppedContainerEpoch(ctx, account.AccountID, r.dataDir)
			if err != nil {
				return fmt.Errorf("revalidate stopped legacy WeCom container after approval consumption: %w", err)
			}
			if !verifiedContainerEpoch.equal(stoppedContainerEpoch) {
				return errors.New("stopped legacy WeCom container execution epoch changed during approval consumption")
			}
		}
		// Approval consumption and live-container proof both bind the
		// original canonical inode. Recheck that exact identity before an
		// ownership helper or either durable profile marker can run.
		if err := verifyCanonicalDataIdentity(r.dataDir, beforeDevice, beforeInode); err != nil {
			return fmt.Errorf("revalidate legacy WeCom data before profile binding: %w", err)
		}

		if !sentinelExists {
			verifiedInfo, err = r.prepareLegacyDataDirectoryForSentinel(
				ctx, account, verifiedData.info, containerRunning,
			)
			if err != nil {
				return err
			}
			verifiedData.info, err = verifiedData.file.Stat()
			if err != nil {
				return fmt.Errorf("refresh pinned legacy WeCom data after ownership migration: %w", err)
			}
			preparedDevice, preparedInode, err := directoryIdentity(verifiedInfo)
			if err != nil || preparedDevice != beforeDevice || preparedInode != beforeInode {
				return errors.New("legacy WeCom Android data changed during ownership migration")
			}
			metadata, err = newWeComProfileMetadata(account.AccountID, r.dataDir, verifiedData.info)
			if err != nil {
				return err
			}
		}
		return publishLegacyWeComProfileBinding(
			account.AccountID, r.dataDir, metadataPath, metadata, verifiedData, !sentinelExists, nil,
		)
	}
	if exists {
		return errors.New("WeCom account container references a missing Android data directory; refusing to rebuild it")
	}

	dataInfo, err = createManagedDirectory(r.dataDir)
	if err != nil {
		return fmt.Errorf("verify initial WeCom Android data directory: %w", err)
	}
	return initializeWeComProfileDocuments(
		account.AccountID, r.dataDir, metadataPath, dataInfo,
	)
}

type profilePublicationHooks struct {
	afterSentinel func()
	afterMetadata func()
}

func publishLegacyWeComProfileBinding(
	accountID, dataDir, metadataPath string,
	metadata weComProfileMetadata,
	data *profileDirectoryAnchor,
	createSentinel bool,
	hooks *profilePublicationHooks,
) (resultErr error) {
	if data == nil || data.file == nil || data.info == nil {
		return errors.New("legacy WeCom Android data directory is not pinned")
	}
	if err := validateWeComProfileMetadata(metadata, accountID, data.info); err != nil {
		return fmt.Errorf("validate legacy WeCom metadata before publication: %w", err)
	}
	if err := validateWeComProfileMetadataPath(metadata, dataDir); err != nil {
		return fmt.Errorf("validate legacy WeCom metadata before publication: %w", err)
	}
	if err := verifyPinnedDirectoryCanonical(
		dataDir, data, "legacy WeCom Android data changed before profile publication",
	); err != nil {
		return err
	}
	stateDirectory, err := openProfileDirectoryWithoutSymlinks(filepath.Dir(filepath.Clean(metadataPath)))
	if err != nil {
		return fmt.Errorf("pin account state directory for legacy WeCom metadata: %w", err)
	}
	defer stateDirectory.close()
	if err := validatePrivateManagedDirectory(stateDirectory, "account state directory"); err != nil {
		return err
	}

	var sentinelPublication *profileDocumentPublication
	var metadataPublication *profileDocumentPublication
	defer func() {
		if resultErr == nil {
			return
		}
		if metadataPublication != nil {
			resultErr = errors.Join(resultErr, metadataPublication.remove())
		}
		if sentinelPublication != nil {
			resultErr = errors.Join(resultErr, sentinelPublication.remove())
		}
	}()

	if createSentinel {
		var err error
		sentinelPublication, err = writeNewProfileDocumentAt(
			data, weComProfileSentinelName, sentinelForMetadata(metadata),
			".wechatcopilot-profile-*.tmp", "WeCom internal profile sentinel",
		)
		if err != nil {
			return fmt.Errorf(
				"write legacy WeCom internal profile sentinel: legacy Android data must allow one-time daemon sentinel binding; refusing to weaken its permissions: %w",
				err,
			)
		}
		if hooks != nil && hooks.afterSentinel != nil {
			hooks.afterSentinel()
		}
	}
	if err := verifyPinnedDirectoryCanonical(
		dataDir, data, "legacy WeCom Android data changed before external metadata publication",
	); err != nil {
		return err
	}

	metadataPublication, err = writeNewProfileDocumentAt(
		stateDirectory, filepath.Base(metadataPath), metadata,
		".wecom-profile-*.tmp", "WeCom profile metadata",
	)
	if err != nil {
		return fmt.Errorf("adopt verified legacy WeCom profile: %w", err)
	}
	if hooks != nil && hooks.afterMetadata != nil {
		hooks.afterMetadata()
	}
	if err := verifyPinnedDirectoryCanonical(
		dataDir, data, "legacy WeCom Android data changed after external metadata publication",
	); err != nil {
		return err
	}
	if err := protectPinnedDataDirectory(dataDir, data); err != nil {
		return err
	}
	return verifyBoundWeComProfileAt(accountID, dataDir, metadata, data)
}

func initializeWeComProfileDocuments(accountID, dataDir, metadataPath string, dataInfo os.FileInfo) error {
	data, err := openProfileDirectoryWithoutSymlinks(dataDir)
	if err != nil {
		return fmt.Errorf("pin initial WeCom Android data directory: %w", err)
	}
	defer data.close()
	expectedDevice, expectedInode, err := directoryIdentity(dataInfo)
	if err != nil {
		return err
	}
	actualDevice, actualInode, err := directoryIdentity(data.info)
	if err != nil || actualDevice != expectedDevice || actualInode != expectedInode {
		return errors.New("initial WeCom Android data directory changed before profile publication")
	}
	return initializeWeComProfileDocumentsAt(accountID, dataDir, metadataPath, data, nil)
}

func initializeWeComProfileDocumentsAt(
	accountID, dataDir, metadataPath string,
	data *profileDirectoryAnchor,
	hooks *profilePublicationHooks,
) error {
	if data == nil || data.file == nil || data.info == nil {
		return errors.New("initial WeCom Android data directory is not pinned")
	}
	entries, err := readPinnedProfileDirectoryAt(data)
	if err != nil {
		return fmt.Errorf("verify initial WeCom Android data directory contents: %w", err)
	}
	if len(entries) != 0 {
		return errors.New("initial WeCom Android data directory is not empty")
	}
	metadata, err := newWeComProfileMetadata(accountID, dataDir, data.info)
	if err != nil {
		return err
	}
	return publishLegacyWeComProfileBinding(
		accountID, dataDir, metadataPath, metadata, data, true, hooks,
	)
}

func (r *Runtime) verifyPersistentProfileBinding(account core.AccountRuntime) (weComProfileMetadata, error) {
	stateDir, err := validateAccountStateDir(account.StateDir, account.AccountID)
	if err != nil {
		return weComProfileMetadata{}, err
	}
	metadata, exists, err := readWeComProfileMetadata(profileMetadataPath(stateDir))
	if err != nil {
		return weComProfileMetadata{}, fmt.Errorf("read WeCom profile metadata: %w", err)
	}
	if !exists {
		return weComProfileMetadata{}, errors.New("initialized WeCom profile metadata is missing")
	}
	metadata, err = verifyAndMigrateWeComProfileMetadata(
		account.AccountID, r.dataDir, profileMetadataPath(stateDir), metadata,
	)
	if err != nil {
		return weComProfileMetadata{}, err
	}
	return metadata, nil
}

func (r *Runtime) verifyProfileBeforeContainerOperation(account core.AccountRuntime, operation string) error {
	if _, err := r.verifyPersistentProfileBinding(account); err != nil {
		return fmt.Errorf("revalidate WeCom profile before container %s: %w", operation, err)
	}
	return nil
}

// verifyRunningContainerProfile proves that a running container's live /data
// mount has the same current device/inode as the pinned host profile, and that
// it exposes the matching private sentinel. Docker's Mount.Source string alone
// is not enough: a bind mount can keep an old inode after its host pathname is
// exchanged.
func (r *Runtime) verifyRunningContainerProfile(
	ctx context.Context,
	account core.AccountRuntime,
) error {
	_, err := r.resolveRunningContainerProfile(ctx, account)
	return err
}

func (r *Runtime) resolveRunningContainerProfile(
	ctx context.Context,
	account core.AccountRuntime,
) (string, error) {
	metadata, err := r.verifyPersistentProfileBinding(account)
	if err != nil {
		return "", fmt.Errorf("revalidate host WeCom profile before container proof: %w", err)
	}
	return r.verifyRunningContainerProfileFrame(ctx, account, metadata, true, "")
}

func (r *Runtime) resolvePinnedRunningContainerProfile(
	ctx context.Context,
	account core.AccountRuntime,
	expectedContainerID string,
) (string, error) {
	if !validImmutableContainerID(expectedContainerID) {
		return "", errors.New("cannot prove a pinned WeCom profile for an invalid container identity")
	}
	metadata, err := r.verifyPersistentProfileBinding(account)
	if err != nil {
		return "", fmt.Errorf("revalidate host WeCom profile before pinned container proof: %w", err)
	}
	return r.verifyRunningContainerProfileFrame(ctx, account, metadata, true, expectedContainerID)
}

func (r *Runtime) verifyRunningLegacyDataIdentity(
	ctx context.Context,
	account core.AccountRuntime,
	dataInfo os.FileInfo,
) error {
	device, inode, err := directoryIdentity(dataInfo)
	if err != nil {
		return err
	}
	metadata := weComProfileMetadata{AccountID: account.AccountID, DataDevice: device, DataInode: inode}
	_, err = r.verifyRunningContainerProfileFrame(ctx, account, metadata, false, "")
	return err
}

func (r *Runtime) verifyRunningContainerProfileFrame(
	ctx context.Context,
	account core.AccountRuntime,
	metadata weComProfileMetadata,
	verifySentinel bool,
	expectedContainerID string,
) (string, error) {
	var containerID string
	var err error
	if expectedContainerID == "" {
		containerID, err = r.inspectExactRunningContainer(ctx, account.AccountID, r.dataDir)
	} else {
		containerID, err = r.inspectExactRunningContainerByID(
			ctx, expectedContainerID, account.AccountID, r.dataDir,
		)
	}
	if err != nil {
		return "", err
	}
	data, err := openProfileDirectoryWithoutSymlinks(r.dataDir)
	if err != nil {
		return "", fmt.Errorf("pin host WeCom profile before live container proof: %w", err)
	}
	defer data.close()
	liveDevice, liveInode, err := directoryIdentity(data.info)
	if err != nil {
		return "", err
	}
	if verifySentinel {
		if err := validateWeComProfileMetadata(metadata, account.AccountID, data.info); err != nil {
			return "", fmt.Errorf("verify host WeCom profile metadata before live container proof: %w", err)
		}
		if err := validateWeComProfileMetadataPath(metadata, r.dataDir); err != nil {
			return "", fmt.Errorf("verify host WeCom profile metadata before live container proof: %w", err)
		}
		var hostSentinel weComProfileSentinel
		exists, err := readPrivateProfileDocumentAt(
			data, weComProfileSentinelName, "host WeCom profile sentinel", &hostSentinel,
		)
		if err != nil {
			return "", fmt.Errorf("verify host WeCom profile sentinel before live container proof: %w", err)
		}
		if !exists {
			return "", errors.New("host WeCom profile sentinel is missing before live container proof")
		}
		if err := validateWeComProfileSentinel(hostSentinel, metadata); err != nil {
			return "", fmt.Errorf("verify host WeCom profile sentinel before live container proof: %w", err)
		}
	} else if metadata.AccountID != account.AccountID ||
		metadata.DataDevice != liveDevice || metadata.DataInode != liveInode {
		return "", errors.New("legacy WeCom Android data changed before live container proof")
	}
	if err := verifyPinnedDirectoryCanonical(
		r.dataDir, data, "host WeCom profile changed before live container proof",
	); err != nil {
		return "", err
	}
	identityOutput, err := r.executor.RunInput(
		ctx, nil, 256,
		r.config.DockerBinary, "container", "exec", "--user", "0:0", containerID,
		"/system/bin/toybox", "stat", "-Lc", "%d:%i", "/data",
	)
	var proofErr error
	if err != nil {
		proofErr = fmt.Errorf("verify running WeCom container data identity: %w", err)
	} else {
		deviceText, inodeText, ok := strings.Cut(strings.TrimSpace(string(identityOutput)), ":")
		if !ok || strings.Contains(inodeText, ":") {
			proofErr = errors.New("running WeCom container returned an invalid data identity")
		} else {
			device, deviceErr := strconv.ParseUint(deviceText, 10, 64)
			inode, inodeErr := strconv.ParseUint(inodeText, 10, 64)
			if deviceErr != nil || inodeErr != nil || device != liveDevice || inode != liveInode {
				proofErr = errors.New("running WeCom container /data does not match the bound host profile device/inode")
			}
		}
	}
	if verifySentinel && proofErr == nil {
		sentinelOutput, err := r.executor.RunInput(
			ctx, nil, maxProfileDocumentBytes+1,
			r.config.DockerBinary, "container", "exec", "--user", "0:0", containerID,
			"/system/bin/toybox", "head", "-c", strconv.Itoa(maxProfileDocumentBytes+1), "/data/"+weComProfileSentinelName,
		)
		if err != nil {
			proofErr = fmt.Errorf("verify running WeCom container profile sentinel: %w", err)
		} else {
			var sentinel weComProfileSentinel
			if err := decodeProfileDocument(sentinelOutput, "running WeCom container profile sentinel", &sentinel); err != nil {
				proofErr = err
			} else if err := validateWeComProfileSentinel(sentinel, metadata); err != nil {
				proofErr = fmt.Errorf("verify running WeCom container profile sentinel: %w", err)
			}
		}
	}
	if err := verifyPinnedDirectoryCanonical(
		r.dataDir, data, "host WeCom profile changed during live container proof",
	); err != nil {
		proofErr = errors.Join(proofErr, err)
	}
	var verifiedContainerID string
	if expectedContainerID == "" {
		verifiedContainerID, err = r.inspectExactRunningContainer(ctx, account.AccountID, r.dataDir)
	} else {
		verifiedContainerID, err = r.inspectExactRunningContainerByID(
			ctx, expectedContainerID, account.AccountID, r.dataDir,
		)
	}
	if err != nil {
		proofErr = errors.Join(proofErr, err)
	} else if verifiedContainerID != containerID {
		proofErr = errors.Join(proofErr, errors.New("running WeCom account container changed during live profile proof"))
	}
	if proofErr != nil {
		return "", proofErr
	}
	return containerID, nil
}

func verifyCanonicalDataIdentity(dataDir string, expectedDevice, expectedInode uint64) error {
	info, exists, err := inspectRealDirectory(dataDir)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("WeCom Android data directory is missing")
	}
	device, inode, err := directoryIdentity(info)
	if err != nil {
		return err
	}
	if device != expectedDevice || inode != expectedInode {
		return errors.New("WeCom Android data directory was replaced or exchanged")
	}
	return nil
}

func verifyBoundWeComProfile(accountID, dataDir string, metadata weComProfileMetadata) error {
	data, err := openProfileDirectoryWithoutSymlinks(dataDir)
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("initialized WeCom Android data directory is missing; refusing to create an empty replacement")
	}
	if err != nil {
		return fmt.Errorf("inspect initialized WeCom Android data directory: %w", err)
	}
	defer data.close()
	return verifyBoundWeComProfileAt(accountID, dataDir, metadata, data)
}

func verifyBoundWeComProfileAt(
	accountID, dataDir string,
	metadata weComProfileMetadata,
	data *profileDirectoryAnchor,
) error {
	if data == nil || data.file == nil || data.info == nil {
		return errors.New("initialized WeCom Android data directory is not pinned")
	}
	if err := validateWeComProfileMetadata(metadata, accountID, data.info); err != nil {
		return fmt.Errorf("verify WeCom profile metadata: %w", err)
	}
	if err := validateWeComProfileMetadataPath(metadata, dataDir); err != nil {
		return fmt.Errorf("verify WeCom profile metadata: %w", err)
	}
	var sentinel weComProfileSentinel
	sentinelExists, err := readPrivateProfileDocumentAt(
		data, weComProfileSentinelName, "WeCom internal profile sentinel", &sentinel,
	)
	if err != nil {
		return fmt.Errorf("read WeCom internal profile sentinel: %w", err)
	}
	if !sentinelExists {
		return errors.New("WeCom internal profile sentinel is missing; Android account data may have been cleared")
	}
	if err := validateWeComProfileSentinel(sentinel, metadata); err != nil {
		return fmt.Errorf("verify WeCom internal profile sentinel: %w", err)
	}
	return verifyPinnedDirectoryCanonical(dataDir, data, "WeCom Android data directory changed while verifying its internal sentinel")
}

func verifyPinnedDirectoryCanonical(path string, pinned *profileDirectoryAnchor, message string) error {
	if pinned == nil || pinned.info == nil {
		return errors.New(message)
	}
	expectedDevice, expectedInode, err := directoryIdentity(pinned.info)
	if err != nil {
		return err
	}
	canonical, err := openProfileDirectoryWithoutSymlinks(path)
	if err != nil {
		return errors.New(message)
	}
	defer canonical.close()
	actualDevice, actualInode, err := directoryIdentity(canonical.info)
	if err != nil || actualDevice != expectedDevice || actualInode != expectedInode {
		return errors.New(message)
	}
	return nil
}

func directoryContainsOnlyProfileSentinel(dataDir string) (bool, error) {
	entries, err := readPinnedProfileDirectory(dataDir)
	if err != nil {
		return false, err
	}
	return len(entries) == 1 && entries[0].Name() == weComProfileSentinelName, nil
}

func directoryIsEmpty(dataDir string) (bool, error) {
	entries, err := readPinnedProfileDirectory(dataDir)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

func readPinnedProfileDirectory(path string) ([]os.DirEntry, error) {
	anchor, err := openProfileDirectoryWithoutSymlinks(path)
	if err != nil {
		return nil, err
	}
	defer anchor.close()
	return readPinnedProfileDirectoryAt(anchor)
}

func readPinnedProfileDirectoryAt(anchor *profileDirectoryAnchor) ([]os.DirEntry, error) {
	if anchor == nil || anchor.file == nil {
		return nil, errors.New("profile directory is not pinned for listing")
	}
	fd, err := unix.Openat(
		int(anchor.file.Fd()), ".",
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(fd), ".")
	if directory == nil {
		_ = unix.Close(fd)
		return nil, errors.New("reopen profile directory for listing returned an invalid descriptor")
	}
	defer directory.Close()
	return directory.ReadDir(-1)
}

func protectDataDirectory(path string) error {
	anchor, err := openProfileDirectoryWithoutSymlinks(path)
	if err != nil {
		return fmt.Errorf("recheck protected account Android data directory: %w", err)
	}
	defer anchor.close()
	return protectPinnedDataDirectory(path, anchor)
}

func protectPinnedDataDirectory(path string, anchor *profileDirectoryAnchor) error {
	if anchor == nil || anchor.file == nil || anchor.info == nil {
		return errors.New("account Android data directory is not pinned")
	}
	stat, ok := anchor.info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot inspect account Android data directory ownership")
	}
	if stat.Uid == uint32(os.Geteuid()) {
		fd, err := unix.Openat(
			int(anchor.file.Fd()), ".",
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		if err != nil {
			return fmt.Errorf("reopen account Android data directory for protection: %w", err)
		}
		if err := unix.Fchmod(fd, 0o700); err != nil {
			_ = unix.Close(fd)
			return fmt.Errorf("protect account Android data directory: %w", err)
		}
		var protected unix.Stat_t
		if err := unix.Fstat(fd, &protected); err != nil {
			_ = unix.Close(fd)
			return err
		}
		if err := unix.Close(fd); err != nil {
			return err
		}
		if protected.Mode&unix.S_IFMT != unix.S_IFDIR || protected.Mode&0o777 != 0o700 {
			return errors.New("account Android data directory mode changed during validation")
		}
		info, err := anchor.file.Stat()
		if err != nil {
			return err
		}
		anchor.info = info
		return verifyPinnedDirectoryCanonical(path, anchor, "account Android data directory changed during protection")
	}

	// Android init owns /data as its numeric system user and can reset the root
	// directory to 0771. Do not require the host daemon to own or chmod that
	// mount root. Its parent remains a private daemon-owned directory, while the
	// external metadata, internal sentinel, and bound inode provide identity.
	parent, err := openProfileDirectoryWithoutSymlinks(filepath.Dir(filepath.Clean(path)))
	if err != nil {
		return fmt.Errorf("open private WeCom profile parent: %w", err)
	}
	defer parent.close()
	if err := validatePrivateManagedDirectory(parent, "WeCom profile parent directory"); err != nil {
		return err
	}
	return verifyPinnedDirectoryCanonical(path, anchor, "account Android data directory changed during protection")
}
