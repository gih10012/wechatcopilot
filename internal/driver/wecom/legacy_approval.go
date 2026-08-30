package wecom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const (
	legacyProfileApprovalPreviousSchema = 2
	legacyProfileApprovalSchemaVersion  = 3
	legacyProfileApprovalName           = "wecom-legacy-profile-approval.json"
)

// legacyProfileApproval is a one-use, operator-created authorization to bind
// one already-existing markerless Android data directory to one saved WeCom
// account. Current records include only the canonical registered data path,
// never profile contents. DataDevice exists solely to decode schema 2; dev_t
// is not stable across legitimate dm-crypt mapper reconstruction.
type legacyProfileApproval struct {
	SchemaVersion         int       `json:"schema_version"`
	AccountID             string    `json:"account_id"`
	DataPath              string    `json:"data_path,omitempty"`
	DataDevice            uint64    `json:"data_device,omitempty"`
	DataInode             uint64    `json:"data_inode"`
	ContainerID           string    `json:"container_id"`
	ContainerState        string    `json:"container_state"`
	ContainerStartedAt    string    `json:"container_started_at"`
	ContainerFinishedAt   string    `json:"container_finished_at"`
	ContainerRestartCount int       `json:"container_restart_count"`
	ContainerExitCode     int       `json:"container_exit_code"`
	CreatedAt             time.Time `json:"created_at"`
}

type legacyProfileApprovalHandle struct {
	directory *profileDirectoryAnchor
	file      *os.File
	stat      unix.Stat_t
	approval  legacyProfileApproval
}

// LegacyProfileApprovalConfig contains only the pinned runtime identity needed
// to prove that an existing legacy account container is exact and stopped.
// APK configuration is deliberately irrelevant to this offline operation.
type LegacyProfileApprovalConfig struct {
	DockerBinary string
	RedroidImage string
	Executor     Executor
}

func (handle *legacyProfileApprovalHandle) close() error {
	if handle == nil {
		return nil
	}
	var err error
	if handle.file != nil {
		err = errors.Join(err, handle.file.Close())
		handle.file = nil
	}
	if handle.directory != nil {
		err = errors.Join(err, handle.directory.close())
		handle.directory = nil
	}
	return err
}

func legacyProfileApprovalPath(stateDir string) string {
	return filepath.Join(stateDir, legacyProfileApprovalName)
}

// CreateStoppedLegacyProfileApproval records an explicit offline operator
// decision for one existing, non-empty WeCom Android data directory whose
// external metadata is missing. A valid internal sentinel may already exist
// after an interrupted migration. The exact account container must remain
// stopped across the publication frame. This function never creates or
// changes /data, the container, profile metadata, or the internal sentinel.
func CreateStoppedLegacyProfileApproval(
	ctx context.Context,
	stateDir, accountID string,
	config LegacyProfileApprovalConfig,
) (resultErr error) {
	return createStoppedLegacyProfileApproval(ctx, stateDir, accountID, config, nil)
}

func createStoppedLegacyProfileApproval(
	ctx context.Context,
	stateDir, accountID string,
	config LegacyProfileApprovalConfig,
	afterPublication func(),
) (resultErr error) {
	stateDir, dataDir, dataInfo, err := inspectMarkerlessLegacyProfile(stateDir, accountID, "approve")
	if err != nil {
		return err
	}
	device, inode, err := directoryIdentity(dataInfo)
	if err != nil {
		return err
	}
	if config.DockerBinary == "" {
		config.DockerBinary = DefaultConfig().DockerBinary
	}
	if !imageDigestPattern.MatchString(config.RedroidImage) {
		return errors.New("legacy WeCom profile approval requires the configured Redroid image pinned by digest")
	}
	if config.Executor == nil {
		config.Executor = OSExecutor{}
	}
	runtime := &Runtime{
		config:        Config{DockerBinary: config.DockerBinary, RedroidImage: config.RedroidImage},
		executor:      config.Executor,
		containerName: containerName(accountID),
		networkName:   networkName(accountID),
		dataDir:       dataDir,
	}
	containerEpoch, err := runtime.inspectExactStoppedContainerEpoch(ctx, accountID, dataDir)
	if err != nil {
		return fmt.Errorf("prove exact stopped legacy WeCom container before approval: %w", err)
	}
	networkExists, err := runtime.inspectNetwork(ctx, accountID)
	if err != nil {
		return fmt.Errorf("prove exact legacy WeCom network before approval: %w", err)
	}
	if !networkExists {
		return errors.New("exact legacy WeCom account network is missing")
	}
	if err := verifyCanonicalDataIdentity(dataDir, device, inode); err != nil {
		return fmt.Errorf("verify legacy WeCom Android data before approval publication: %w", err)
	}
	handle, err := publishLegacyProfileApproval(stateDir, accountID, dataDir, inode, containerEpoch)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, handle.unlink())
		}
		_ = handle.close()
	}()
	if afterPublication != nil {
		afterPublication()
	}
	if err := verifyCanonicalDataIdentity(dataDir, device, inode); err != nil {
		return fmt.Errorf("legacy WeCom Android data changed while approval was recorded: %w", err)
	}
	verifiedContainerEpoch, err := runtime.inspectExactStoppedContainerEpoch(ctx, accountID, dataDir)
	if err != nil {
		return fmt.Errorf("revalidate exact stopped legacy WeCom container after approval: %w", err)
	}
	if !verifiedContainerEpoch.equal(containerEpoch) {
		return errors.New("exact stopped legacy WeCom container execution epoch changed during approval")
	}
	return nil
}

// createLegacyProfileApproval is the filesystem-only primitive used by unit
// tests and by no production command. Production approval must call
// CreateStoppedLegacyProfileApproval so a running or foreign container cannot
// acquire an offline authorization record.
func createLegacyProfileApproval(
	stateDir, accountID string,
	containerEpoch containerExecutionEpoch,
) (resultErr error) {
	return createLegacyProfileApprovalWithHook(stateDir, accountID, containerEpoch, nil)
}

func createLegacyProfileApprovalWithHook(
	stateDir, accountID string,
	containerEpoch containerExecutionEpoch,
	afterPublication func(),
) (resultErr error) {
	stateDir, dataDir, dataInfo, err := inspectMarkerlessLegacyProfile(stateDir, accountID, "approve")
	if err != nil {
		return err
	}
	device, inode, err := directoryIdentity(dataInfo)
	if err != nil {
		return err
	}
	handle, err := publishLegacyProfileApproval(stateDir, accountID, dataDir, inode, containerEpoch)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, handle.unlink())
		}
		_ = handle.close()
	}()
	if afterPublication != nil {
		afterPublication()
	}
	if err := verifyCanonicalDataIdentity(dataDir, device, inode); err != nil {
		return fmt.Errorf("legacy WeCom Android data changed while approval was recorded: %w", err)
	}
	return nil
}

func publishLegacyProfileApproval(
	stateDir, accountID, dataDir string,
	inode uint64,
	containerEpoch containerExecutionEpoch,
) (*legacyProfileApprovalHandle, error) {
	approval := legacyProfileApproval{
		SchemaVersion:         legacyProfileApprovalSchemaVersion,
		AccountID:             accountID,
		DataPath:              filepath.Clean(dataDir),
		DataInode:             inode,
		ContainerID:           containerEpoch.ID,
		ContainerState:        containerEpoch.Status,
		ContainerStartedAt:    containerEpoch.StartedAt,
		ContainerFinishedAt:   containerEpoch.FinishedAt,
		ContainerRestartCount: containerEpoch.RestartCount,
		ContainerExitCode:     containerEpoch.ExitCode,
		CreatedAt:             time.Now().UTC(),
	}
	directory, err := openProfileDirectoryWithoutSymlinks(stateDir)
	if err != nil {
		return nil, err
	}
	if err := validatePrivateManagedDirectory(directory, "legacy WeCom profile approval directory"); err != nil {
		_ = directory.close()
		return nil, err
	}
	publication, err := writeNewProfileDocumentAt(
		directory, legacyProfileApprovalName, approval,
		".wecom-legacy-profile-approval-*.tmp", "legacy WeCom profile approval",
	)
	if err != nil {
		if publication != nil {
			err = errors.Join(err, publication.remove())
		}
		_ = directory.close()
		return nil, err
	}
	handle, exists, err := openLegacyProfileApproval(stateDir)
	if err != nil {
		err = errors.Join(err, publication.remove())
		_ = directory.close()
		return nil, err
	}
	if !exists {
		err = errors.Join(errors.New("legacy WeCom profile approval disappeared after publication"), publication.remove())
		_ = directory.close()
		return nil, err
	}
	expectedDevice, expectedInode, identityErr := directoryIdentity(directory.info)
	actualDevice, actualInode, actualIdentityErr := directoryIdentity(handle.directory.info)
	if identityErr != nil || actualIdentityErr != nil || expectedDevice != actualDevice || expectedInode != actualInode {
		_ = handle.close()
		err = errors.Join(errors.New("legacy WeCom profile approval directory changed during publication"), publication.remove())
		_ = directory.close()
		return nil, err
	}
	if err := validateLegacyProfileApproval(handle.approval, accountID, dataDir, inode, containerEpoch); err != nil {
		_ = handle.close()
		err = errors.Join(err, publication.remove())
		_ = directory.close()
		return nil, err
	}
	_ = directory.close()
	return handle, nil
}

// consumeLegacyProfileApprovalForData validates and removes exactly one
// account-, inode-, and stopped-container-epoch-bound approval.
func consumeLegacyProfileApprovalForData(
	stateDir, accountID, dataDir string,
	containerEpoch containerExecutionEpoch,
) (resultErr error) {
	validatedStateDir, expectedDataDir, dataInfo, err := inspectMarkerlessLegacyProfile(stateDir, accountID, "consume")
	if err != nil {
		return err
	}
	if filepath.Clean(dataDir) != expectedDataDir {
		return errors.New("legacy WeCom approval data path is not the registered account data directory")
	}
	device, inode, err := directoryIdentity(dataInfo)
	if err != nil {
		return err
	}

	handle, exists, err := openLegacyProfileApproval(validatedStateDir)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("explicit offline legacy WeCom profile approval is missing")
	}
	defer func() {
		resultErr = errors.Join(resultErr, handle.close())
	}()
	if err := validateLegacyProfileApprovalIdentity(handle.approval, accountID); err != nil {
		return err
	}
	if err := validateLegacyProfileApprovalPath(handle.approval, expectedDataDir); err != nil {
		return err
	}
	if handle.approval.DataInode != inode {
		return errors.New("legacy WeCom Android data directory was replaced or exchanged after approval")
	}
	if !handle.approval.containerEpoch().equal(containerEpoch) {
		staleErr := errors.New("legacy WeCom container ran or changed after approval; the stale approval was revoked and must be recreated")
		return errors.Join(staleErr, handle.unlink())
	}
	if err := verifyCanonicalDataIdentity(expectedDataDir, device, inode); err != nil {
		return fmt.Errorf("legacy WeCom Android data changed before approval consumption: %w", err)
	}

	// The state directory is mode 0700 and owned by this process. Revalidate
	// that the directory entry is still the single-link regular file opened
	// above, then unlink it through the pinned dirfd and durably sync the
	// directory. No pathname supplied by the caller participates in deletion.
	var current unix.Stat_t
	if err := unix.Fstatat(
		int(handle.directory.file.Fd()), legacyProfileApprovalName, &current, unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return fmt.Errorf("revalidate legacy WeCom profile approval: %w", err)
	}
	if !sameLegacyApprovalFile(handle.stat, current) {
		return errors.New("legacy WeCom profile approval changed during validation")
	}
	return handle.unlink()
}

// consumeLegacyProfileApprovalIfPresent revokes any structurally valid
// same-account approval after an independent live proof. It intentionally does
// not require the old approved inode to match the live inode: retaining an old
// approval would make it replayable if that directory later returned.
func consumeLegacyProfileApprovalIfPresent(stateDir, accountID, dataDir string) (bool, error) {
	validatedStateDir, expectedDataDir, _, err := inspectMarkerlessLegacyProfile(stateDir, accountID, "revoke")
	if err != nil {
		return false, err
	}
	if filepath.Clean(dataDir) != expectedDataDir {
		return false, errors.New("legacy WeCom approval data path is not the registered account data directory")
	}
	handle, exists, err := openLegacyProfileApproval(validatedStateDir)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	defer handle.close()
	if err := validateLegacyProfileApprovalIdentity(handle.approval, accountID); err != nil {
		return false, err
	}
	if err := validateLegacyProfileApprovalPath(handle.approval, expectedDataDir); err != nil {
		return false, err
	}
	if err := handle.unlink(); err != nil {
		return false, err
	}
	return true, nil
}

func (handle *legacyProfileApprovalHandle) unlink() error {
	if handle == nil || handle.directory == nil || handle.directory.file == nil || handle.file == nil {
		return errors.New("legacy WeCom profile approval is not pinned")
	}
	var current unix.Stat_t
	if err := unix.Fstatat(
		int(handle.directory.file.Fd()), legacyProfileApprovalName, &current, unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("revalidate legacy WeCom profile approval: %w", err)
	}
	if !sameLegacyApprovalFile(handle.stat, current) {
		return errors.New("legacy WeCom profile approval changed during validation")
	}
	if err := unix.Unlinkat(int(handle.directory.file.Fd()), legacyProfileApprovalName, 0); err != nil {
		return fmt.Errorf("consume legacy WeCom profile approval: %w", err)
	}
	if err := syncProfileDirectory(handle.directory); err != nil {
		return fmt.Errorf("persist legacy WeCom profile approval consumption: %w", err)
	}
	return nil
}

func inspectMarkerlessLegacyProfile(stateDir, accountID, operation string) (string, string, os.FileInfo, error) {
	stateDir, err := validateAccountStateDir(stateDir, accountID)
	if err != nil {
		return "", "", nil, err
	}
	dataDir, err := accountDataDir(stateDir, accountID)
	if err != nil {
		return "", "", nil, err
	}

	// The Android root may be owned by an Android numeric UID and have mode
	// 0771. Its daemon-owned mode-0700 parent is therefore the confidentiality
	// boundary that must already exist; this function never chmods or creates it.
	dataParent, err := openProfileDirectoryWithoutSymlinks(filepath.Dir(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", nil, errors.New("legacy WeCom profile parent directory is missing")
	}
	if err != nil {
		return "", "", nil, fmt.Errorf("inspect legacy WeCom profile parent directory: %w", err)
	}
	if err := validatePrivateManagedDirectory(dataParent, "legacy WeCom profile parent directory"); err != nil {
		_ = dataParent.close()
		return "", "", nil, err
	}
	data, err := openProfileChildDirectory(dataParent, filepath.Base(dataDir))
	if err != nil {
		_ = dataParent.close()
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", nil, errors.New("legacy WeCom Android data directory is missing")
		}
		return "", "", nil, fmt.Errorf("inspect legacy WeCom Android data directory: %w", err)
	}
	defer dataParent.close()
	defer data.close()
	dataInfo := data.info
	entries, err := readPinnedProfileDirectoryAt(data)
	if err != nil {
		return "", "", nil, fmt.Errorf("inspect legacy WeCom Android data contents: %w", err)
	}
	if len(entries) == 0 {
		return "", "", nil, errors.New("legacy WeCom Android data directory is empty and does not require approval")
	}

	if metadata, exists, err := readWeComProfileMetadata(profileMetadataPath(stateDir)); err != nil {
		return "", "", nil, fmt.Errorf("inspect WeCom profile metadata before approval %s: %w", operation, err)
	} else if exists {
		_ = metadata
		return "", "", nil, errors.New("WeCom profile metadata already exists")
	}
	var sentinel weComProfileSentinel
	if exists, err := readPrivateProfileDocumentAt(
		data, weComProfileSentinelName, "WeCom profile sentinel", &sentinel,
	); err != nil {
		return "", "", nil, fmt.Errorf("inspect WeCom profile sentinel before approval %s: %w", operation, err)
	} else if exists {
		if err := validateStandaloneProfileSentinel(sentinel, accountID); err != nil {
			return "", "", nil, fmt.Errorf("verify WeCom profile sentinel before approval %s: %w", operation, err)
		}
	}
	if err := verifyPinnedDirectoryCanonical(
		dataDir, data, "legacy WeCom Android data changed during approval inspection",
	); err != nil {
		return "", "", nil, err
	}
	return stateDir, dataDir, dataInfo, nil
}

func openLegacyProfileApproval(stateDir string) (*legacyProfileApprovalHandle, bool, error) {
	directory, err := openProfileDirectoryWithoutSymlinks(stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open legacy WeCom profile approval directory: %w", err)
	}
	if err := validatePrivateManagedDirectory(directory, "legacy WeCom profile approval directory"); err != nil {
		_ = directory.close()
		return nil, false, err
	}
	fd, err := unix.Openat(
		int(directory.file.Fd()), legacyProfileApprovalName,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if errors.Is(err, unix.ENOENT) {
		_ = directory.close()
		return nil, false, nil
	}
	if err != nil {
		_ = directory.close()
		return nil, true, fmt.Errorf("open legacy WeCom profile approval without following symlinks: %w", err)
	}
	file := os.NewFile(uintptr(fd), legacyProfileApprovalName)
	if file == nil {
		_ = unix.Close(fd)
		_ = directory.close()
		return nil, true, errors.New("open legacy WeCom profile approval returned an invalid descriptor")
	}
	handle := &legacyProfileApprovalHandle{directory: directory, file: file}
	if err := unix.Fstat(fd, &handle.stat); err != nil {
		_ = handle.close()
		return nil, true, err
	}
	if handle.stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		handle.stat.Uid != uint32(os.Geteuid()) ||
		handle.stat.Nlink != 1 ||
		handle.stat.Mode&0o777 != 0o600 ||
		handle.stat.Size <= 0 || handle.stat.Size > maxProfileDocumentBytes {
		_ = handle.close()
		return nil, true, errors.New("legacy WeCom profile approval must be a mode-0600 single-link regular file owned by the daemon user")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxProfileDocumentBytes+1))
	if err != nil {
		_ = handle.close()
		return nil, true, err
	}
	if len(contents) == 0 || len(contents) > maxProfileDocumentBytes {
		_ = handle.close()
		return nil, true, errors.New("legacy WeCom profile approval has an invalid size")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&handle.approval); err != nil {
		_ = handle.close()
		return nil, true, fmt.Errorf("decode legacy WeCom profile approval: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		_ = handle.close()
		if err == nil {
			return nil, true, errors.New("decode legacy WeCom profile approval: trailing JSON value")
		}
		return nil, true, fmt.Errorf("decode legacy WeCom profile approval: %w", err)
	}
	return handle, true, nil
}

func (approval legacyProfileApproval) containerEpoch() containerExecutionEpoch {
	return containerExecutionEpoch{
		ID:           approval.ContainerID,
		Status:       approval.ContainerState,
		StartedAt:    approval.ContainerStartedAt,
		FinishedAt:   approval.ContainerFinishedAt,
		RestartCount: approval.ContainerRestartCount,
		ExitCode:     approval.ContainerExitCode,
	}
}

func validateLegacyProfileApprovalIdentity(approval legacyProfileApproval, accountID string) error {
	if approval.SchemaVersion != legacyProfileApprovalPreviousSchema && approval.SchemaVersion != legacyProfileApprovalSchemaVersion {
		return fmt.Errorf("unsupported legacy WeCom profile approval schema %d", approval.SchemaVersion)
	}
	if approval.AccountID != accountID {
		return errors.New("legacy WeCom profile approval belongs to another account")
	}
	if approval.DataInode == 0 || approval.CreatedAt.IsZero() ||
		!validImmutableContainerID(approval.ContainerID) || approval.ContainerRestartCount < 0 {
		return errors.New("legacy WeCom profile approval has invalid identity fields")
	}
	switch approval.SchemaVersion {
	case legacyProfileApprovalPreviousSchema:
		if approval.DataDevice == 0 || approval.DataPath != "" {
			return errors.New("legacy WeCom profile approval has invalid schema-2 identity fields")
		}
	case legacyProfileApprovalSchemaVersion:
		if approval.DataDevice != 0 || !canonicalAbsoluteProfilePath(approval.DataPath) {
			return errors.New("legacy WeCom profile approval has an invalid canonical data path")
		}
	}
	if approval.ContainerState != "created" && approval.ContainerState != "exited" {
		return errors.New("legacy WeCom profile approval has an invalid stopped container state")
	}
	if _, err := time.Parse(time.RFC3339Nano, approval.ContainerStartedAt); err != nil {
		return errors.New("legacy WeCom profile approval has an invalid container start epoch")
	}
	if _, err := time.Parse(time.RFC3339Nano, approval.ContainerFinishedAt); err != nil {
		return errors.New("legacy WeCom profile approval has an invalid container finish epoch")
	}
	return nil
}

func validateLegacyProfileApproval(
	approval legacyProfileApproval,
	accountID, dataDir string,
	inode uint64,
	containerEpoch containerExecutionEpoch,
) error {
	if err := validateLegacyProfileApprovalIdentity(approval, accountID); err != nil {
		return err
	}
	if err := validateLegacyProfileApprovalPath(approval, dataDir); err != nil {
		return err
	}
	if approval.DataInode != inode {
		return errors.New("legacy WeCom Android data directory was replaced or exchanged after approval")
	}
	if !approval.containerEpoch().equal(containerEpoch) {
		return errors.New("legacy WeCom container execution epoch does not match the approval")
	}
	return nil
}

func validateLegacyProfileApprovalPath(approval legacyProfileApproval, dataDir string) error {
	if approval.SchemaVersion == legacyProfileApprovalPreviousSchema {
		// Schema 2 predates the path field. Its containing registered account
		// directory and accountDataDir derivation are the canonical path binding.
		return nil
	}
	if !canonicalAbsoluteProfilePath(dataDir) || approval.DataPath != filepath.Clean(dataDir) {
		return errors.New("legacy WeCom profile approval belongs to another canonical data path")
	}
	return nil
}

func sameLegacyApprovalFile(expected, actual unix.Stat_t) bool {
	return expected.Dev == actual.Dev && expected.Ino == actual.Ino &&
		actual.Mode&unix.S_IFMT == unix.S_IFREG &&
		actual.Uid == uint32(os.Geteuid()) && actual.Nlink == 1 &&
		actual.Mode&0o777 == 0o600 && actual.Size == expected.Size
}
