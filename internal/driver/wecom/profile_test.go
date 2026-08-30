package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	core "github.com/gih10012/wechatcopilot/internal/driver"
)

func TestPersistentProfileFirstInitializationAndRestart(t *testing.T) {
	account := profileTestAccount(t, "work")
	firstExecutor := &sequenceExecutor{results: []executorResult{
		{err: errors.New("container not found")},
		{output: nil},
	}}
	first := profileTestRuntime(t, account, firstExecutor)
	if err := first.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	dataInfo, exists, err := inspectRealDirectory(first.dataDir)
	if err != nil || !exists {
		t.Fatalf("initial data directory: exists=%v err=%v", exists, err)
	}
	metadata, exists, err := readWeComProfileMetadata(profileMetadataPath(account.StateDir))
	if err != nil || !exists {
		t.Fatalf("initial profile metadata: exists=%v err=%v", exists, err)
	}
	if err := validateWeComProfileMetadata(metadata, account.AccountID, dataInfo); err != nil {
		t.Fatal(err)
	}
	sentinel, sentinelExists, err := readWeComProfileSentinel(profileSentinelPath(first.dataDir))
	if err != nil || !sentinelExists {
		t.Fatalf("initial internal sentinel: exists=%v err=%v", sentinelExists, err)
	}
	if err := validateWeComProfileSentinel(sentinel, metadata); err != nil {
		t.Fatal(err)
	}
	metadataInfo, err := os.Stat(profileMetadataPath(account.StateDir))
	if err != nil || metadataInfo.Mode().Perm() != 0o600 {
		t.Fatalf("profile metadata permissions: info=%v err=%v", metadataInfo, err)
	}
	metadataDocument, err := os.ReadFile(profileMetadataPath(account.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(metadataDocument), `"data_device"`) ||
		!strings.Contains(string(metadataDocument), `"data_path"`) {
		t.Fatalf("current metadata persisted a transient device or omitted its canonical path: %s", metadataDocument)
	}

	secondExecutor := &sequenceExecutor{results: []executorResult{
		{err: errors.New("container not found")},
		{output: nil},
	}}
	second := profileTestRuntime(t, account, secondExecutor)
	if err := second.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	reloaded, _, err := readWeComProfileMetadata(profileMetadataPath(account.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ProfileID != metadata.ProfileID || reloaded.DataInode != metadata.DataInode || reloaded.DataDevice != metadata.DataDevice {
		t.Fatalf("restart changed stable profile identity: before=%#v after=%#v", metadata, reloaded)
	}
}

func TestPersistentProfileMigratesSchemaOneAcrossDeviceRenumbering(t *testing.T) {
	account := profileTestAccount(t, "work")
	initializer := profileTestRuntime(t, account, &sequenceExecutor{results: []executorResult{
		{err: errors.New("container not found")},
		{output: nil},
	}})
	if err := initializer.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	metadataPath := profileMetadataPath(account.StateDir)
	metadata, exists, err := readWeComProfileMetadata(metadataPath)
	if err != nil || !exists {
		t.Fatalf("read initialized profile metadata: exists=%v err=%v", exists, err)
	}
	dataInfo, err := os.Stat(initializer.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	device, _, err := directoryIdentity(dataInfo)
	if err != nil {
		t.Fatal(err)
	}
	legacy := metadata
	legacy.SchemaVersion = legacyWeComProfileSchemaVersion
	legacy.DataPath = ""
	legacy.DataDevice = device + 1 // Simulate a reconstructed dm-crypt mapper.
	if err := os.Remove(metadataPath); err != nil {
		t.Fatal(err)
	}
	if err := writeNewWeComProfileMetadata(metadataPath, legacy); err != nil {
		t.Fatal(err)
	}

	restarted := profileTestRuntime(t, account, &sequenceExecutor{results: []executorResult{
		{err: errors.New("container not found")},
		{output: nil},
	}})
	if err := restarted.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatalf("recover profile after device renumbering: %v", err)
	}
	migrated, exists, err := readWeComProfileMetadata(metadataPath)
	if err != nil || !exists {
		t.Fatalf("read migrated profile metadata: exists=%v err=%v", exists, err)
	}
	if migrated.SchemaVersion != weComProfileSchemaVersion || migrated.DataDevice != 0 ||
		migrated.DataPath != filepath.Clean(initializer.dataDir) ||
		migrated.DataInode != legacy.DataInode || migrated.ProfileID != legacy.ProfileID ||
		!migrated.CreatedAt.Equal(legacy.CreatedAt) {
		t.Fatalf("schema-1 migration changed durable profile identity: before=%#v after=%#v", legacy, migrated)
	}
}

func TestPersistentProfileMigrationStillRejectsChangedInode(t *testing.T) {
	account := profileTestAccount(t, "work")
	initializer := profileTestRuntime(t, account, &sequenceExecutor{results: []executorResult{
		{err: errors.New("container not found")},
		{output: nil},
	}})
	if err := initializer.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	metadataPath := profileMetadataPath(account.StateDir)
	metadata, _, err := readWeComProfileMetadata(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	dataInfo, err := os.Stat(initializer.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	device, _, err := directoryIdentity(dataInfo)
	if err != nil {
		t.Fatal(err)
	}
	legacy := metadata
	legacy.SchemaVersion = legacyWeComProfileSchemaVersion
	legacy.DataPath = ""
	legacy.DataDevice = device + 1
	if err := os.Remove(metadataPath); err != nil {
		t.Fatal(err)
	}
	if err := writeNewWeComProfileMetadata(metadataPath, legacy); err != nil {
		t.Fatal(err)
	}
	sentinel, err := os.ReadFile(profileSentinelPath(initializer.dataDir))
	if err != nil {
		t.Fatal(err)
	}
	original := initializer.dataDir + ".original"
	if err := os.Rename(initializer.dataDir, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(initializer.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profileSentinelPath(initializer.dataDir), sentinel, 0o600); err != nil {
		t.Fatal(err)
	}

	restarted := profileTestRuntime(t, account, &sequenceExecutor{})
	err = restarted.preparePersistentProfile(context.Background(), account)
	if err == nil || !strings.Contains(err.Error(), "replaced or exchanged") {
		t.Fatalf("schema-1 migration accepted a different inode: %v", err)
	}
	stored, _, readErr := readWeComProfileMetadata(metadataPath)
	if readErr != nil || stored.SchemaVersion != legacyWeComProfileSchemaVersion {
		t.Fatalf("rejected migration modified legacy metadata: metadata=%#v err=%v", stored, readErr)
	}
}

func TestPersistentProfileRejectsDifferentCanonicalPath(t *testing.T) {
	account := profileTestAccount(t, "work")
	initializer := profileTestRuntime(t, account, &sequenceExecutor{results: []executorResult{
		{err: errors.New("container not found")},
		{output: nil},
	}})
	if err := initializer.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	metadataPath := profileMetadataPath(account.StateDir)
	metadata, _, err := readWeComProfileMetadata(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata.DataPath = filepath.Join(filepath.Dir(initializer.dataDir), "different-data")
	if err := os.Remove(metadataPath); err != nil {
		t.Fatal(err)
	}
	if err := writeNewWeComProfileMetadata(metadataPath, metadata); err != nil {
		t.Fatal(err)
	}

	restarted := profileTestRuntime(t, account, &sequenceExecutor{})
	err = restarted.preparePersistentProfile(context.Background(), account)
	if err == nil || !strings.Contains(err.Error(), "another canonical data path") {
		t.Fatalf("different-path profile metadata error = %v", err)
	}
}

func TestPersistentProfileRejectsUnknownMetadataSchemaWithoutMigration(t *testing.T) {
	account := profileTestAccount(t, "work")
	initializer := profileTestRuntime(t, account, &sequenceExecutor{results: []executorResult{
		{err: errors.New("container not found")},
		{output: nil},
	}})
	if err := initializer.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	metadataPath := profileMetadataPath(account.StateDir)
	metadata, _, err := readWeComProfileMetadata(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata.SchemaVersion = weComProfileSchemaVersion + 1
	if err := os.Remove(metadataPath); err != nil {
		t.Fatal(err)
	}
	if err := writeNewWeComProfileMetadata(metadataPath, metadata); err != nil {
		t.Fatal(err)
	}

	restarted := profileTestRuntime(t, account, &sequenceExecutor{})
	err = restarted.preparePersistentProfile(context.Background(), account)
	if err == nil || !strings.Contains(err.Error(), "unsupported WeCom profile metadata schema") {
		t.Fatalf("unknown metadata schema error = %v", err)
	}
	stored, exists, readErr := readWeComProfileMetadata(metadataPath)
	if readErr != nil || !exists || stored.SchemaVersion != metadata.SchemaVersion {
		t.Fatalf("unknown metadata was modified: exists=%v metadata=%#v err=%v", exists, stored, readErr)
	}
}

func TestPinnedProfileFrameRejectsCanonicalExchangeWithoutDurableDeviceBinding(t *testing.T) {
	account := profileTestAccount(t, "work")
	initializer := profileTestRuntime(t, account, &sequenceExecutor{results: []executorResult{
		{err: errors.New("container not found")},
		{output: nil},
	}})
	if err := initializer.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	metadata, _, err := readWeComProfileMetadata(profileMetadataPath(account.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := openProfileDirectoryWithoutSymlinks(initializer.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.close()
	sentinel, err := os.ReadFile(profileSentinelPath(initializer.dataDir))
	if err != nil {
		t.Fatal(err)
	}
	replacement := initializer.dataDir + ".replacement"
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profileSentinelPath(replacement), sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	original := initializer.dataDir + ".original"
	if err := os.Rename(initializer.dataDir, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, initializer.dataDir); err != nil {
		t.Fatal(err)
	}
	if err := verifyBoundWeComProfileAt(account.AccountID, initializer.dataDir, metadata, pinned); err == nil ||
		!strings.Contains(err.Error(), "changed while verifying") {
		t.Fatalf("pinned profile frame accepted canonical path exchange: %v", err)
	}
}

func TestPersistentProfileAdoptsOnlyExactLegacyContainer(t *testing.T) {
	account := profileTestAccount(t, "work")
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dataDir, "existing-login-state")
	if err := os.WriteFile(sentinel, []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataInfo, err := os.Stat(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	device, inode, err := directoryIdentity(dataInfo)
	if err != nil {
		t.Fatal(err)
	}
	config := validTestConfig(t)
	executor := &sequenceExecutor{results: []executorResult{
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
		{output: networkInspection(t, account.AccountID)},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
		{output: []byte(fmt.Sprintf("%d:%d\n", device, inode))},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
	}}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	configureProfileTestRuntime(runtime, account, dataDir)
	if err := runtime.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(sentinel); err != nil || string(contents) != "preserve me" {
		t.Fatalf("legacy adoption modified data: contents=%q err=%v", contents, err)
	}
	metadata, exists, err := readWeComProfileMetadata(profileMetadataPath(account.StateDir))
	if err != nil || !exists || metadata.AccountID != account.AccountID {
		t.Fatalf("legacy profile was not bound: metadata=%#v exists=%v err=%v", metadata, exists, err)
	}
	sentinelRecord, sentinelExists, err := readWeComProfileSentinel(profileSentinelPath(dataDir))
	if err != nil || !sentinelExists {
		t.Fatalf("legacy profile internal sentinel: exists=%v err=%v", sentinelExists, err)
	}
	if err := validateWeComProfileSentinel(sentinelRecord, metadata); err != nil {
		t.Fatal(err)
	}
}

func TestRunningLegacyProfileInodeMismatchWritesNoMarkers(t *testing.T) {
	account := profileTestAccount(t, "work")
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyState := filepath.Join(dataDir, "existing-login-state")
	if err := os.WriteFile(legacyState, []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataInfo, err := os.Stat(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	device, inode, err := directoryIdentity(dataInfo)
	if err != nil {
		t.Fatal(err)
	}
	config := validTestConfig(t)
	executor := &sequenceExecutor{results: []executorResult{
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
		{output: networkInspection(t, account.AccountID)},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
		{output: []byte(fmt.Sprintf("%d:%d\n", device, inode+1))},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
	}}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	configureProfileTestRuntime(runtime, account, dataDir)
	if err := runtime.preparePersistentProfile(context.Background(), account); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("live legacy inode mismatch error = %v", err)
	}
	if _, err := os.Stat(profileSentinelPath(dataDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live inode mismatch wrote internal marker: %v", err)
	}
	if _, err := os.Stat(profileMetadataPath(account.StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live inode mismatch wrote external marker: %v", err)
	}
	if contents, err := os.ReadFile(legacyState); err != nil || string(contents) != "preserve me" {
		t.Fatalf("live inode mismatch modified legacy state: contents=%q err=%v", contents, err)
	}
}

func TestStoppedLegacySentinelStillRequiresExplicitApproval(t *testing.T) {
	account := profileTestAccount(t, "work")
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "existing-login-state"), []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataInfo, err := os.Stat(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := newWeComProfileMetadata(account.AccountID, dataDir, dataInfo)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeNewWeComProfileSentinel(profileSentinelPath(dataDir), sentinelForMetadata(metadata)); err != nil {
		t.Fatal(err)
	}
	config := validTestConfig(t)
	executor := &sequenceExecutor{results: []executorResult{
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, false, false)},
		{output: networkInspection(t, account.AccountID)},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, false, false)},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, false, false)},
	}}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	configureProfileTestRuntime(runtime, account, dataDir)
	err = runtime.preparePersistentProfile(context.Background(), account)
	if err == nil || !strings.Contains(err.Error(), "explicit offline") {
		t.Fatalf("stopped legacy sentinel approval error = %v", err)
	}
	if _, err := os.Lstat(profileMetadataPath(account.StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unapproved stopped sentinel published metadata: %v", err)
	}
	stored, exists, err := readWeComProfileSentinel(profileSentinelPath(dataDir))
	if err != nil || !exists || stored.ProfileID != metadata.ProfileID {
		t.Fatalf("pre-existing sentinel changed: exists=%v sentinel=%#v err=%v", exists, stored, err)
	}
}

func TestRunningLegacySentinelRequiresFullLiveProof(t *testing.T) {
	account := profileTestAccount(t, "work")
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "existing-login-state"), []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataInfo, err := os.Stat(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := newWeComProfileMetadata(account.AccountID, dataDir, dataInfo)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeNewWeComProfileSentinel(profileSentinelPath(dataDir), sentinelForMetadata(metadata)); err != nil {
		t.Fatal(err)
	}
	config := validTestConfig(t)
	executor := &sequenceExecutor{results: []executorResult{
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
		{output: networkInspection(t, account.AccountID)},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
		{output: []byte(fmt.Sprintf("%d:%d\n", metadata.DataDevice, metadata.DataInode+1))},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
	}}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	configureProfileTestRuntime(runtime, account, dataDir)
	err = runtime.preparePersistentProfile(context.Background(), account)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("running legacy sentinel live-proof error = %v", err)
	}
	if _, err := os.Lstat(profileMetadataPath(account.StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed live proof published metadata: %v", err)
	}
}

func TestRunningLegacyAdoptionRevokesMatchingStoppedApproval(t *testing.T) {
	account := profileTestAccount(t, "work")
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "existing-login-state"), []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := createLegacyProfileApproval(account.StateDir, account.AccountID, testContainerExecutionEpoch(account.AccountID)); err != nil {
		t.Fatal(err)
	}
	dataInfo, err := os.Stat(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	device, inode, err := directoryIdentity(dataInfo)
	if err != nil {
		t.Fatal(err)
	}
	config := validTestConfig(t)
	executor := &sequenceExecutor{results: []executorResult{
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
		{output: networkInspection(t, account.AccountID)},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
		{output: []byte(fmt.Sprintf("%d:%d\n", device, inode))},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
	}}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	configureProfileTestRuntime(runtime, account, dataDir)
	if err := runtime.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatalf("live adopt legacy profile with superseded approval: %v", err)
	}
	if _, err := os.Lstat(legacyProfileApprovalPath(account.StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live adoption left replayable approval: %v", err)
	}
}

func TestRunningLegacyAdoptionRevokesApprovalForPreviousInode(t *testing.T) {
	account := profileTestAccount(t, "work")
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "old-login-state"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := createLegacyProfileApproval(
		account.StateDir, account.AccountID, testContainerExecutionEpoch(account.AccountID),
	); err != nil {
		t.Fatal(err)
	}
	oldDataDir := dataDir + ".old-approved"
	if err := os.Rename(dataDir, oldDataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "current-login-state"), []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataInfo, err := os.Stat(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	device, inode, err := directoryIdentity(dataInfo)
	if err != nil {
		t.Fatal(err)
	}
	config := validTestConfig(t)
	executor := &sequenceExecutor{results: []executorResult{
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
		{output: networkInspection(t, account.AccountID)},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
		{output: []byte(fmt.Sprintf("%d:%d\n", device, inode))},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false)},
	}}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	configureProfileTestRuntime(runtime, account, dataDir)
	if err := runtime.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatalf("live adoption with previous-inode approval: %v", err)
	}
	if _, err := os.Lstat(legacyProfileApprovalPath(account.StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live adoption retained previous-inode approval: %v", err)
	}
	if _, err := os.Lstat(profileSentinelPath(oldDataDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live adoption mutated previously approved directory: %v", err)
	}
	if _, err := os.Lstat(profileSentinelPath(dataDir)); err != nil {
		t.Fatalf("live adoption did not bind current directory: %v", err)
	}
}

func TestStoppedMarkerlessLegacyProfileRequiresExplicitApproval(t *testing.T) {
	account := profileTestAccount(t, "work")
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "existing-login-state"), []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := validTestConfig(t)
	executor := &sequenceExecutor{results: []executorResult{
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, false, false)},
		{output: networkInspection(t, account.AccountID)},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, false, false)},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, false, false)},
	}}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	configureProfileTestRuntime(runtime, account, dataDir)
	if err := runtime.preparePersistentProfile(context.Background(), account); err == nil || !strings.Contains(err.Error(), "explicit offline") {
		t.Fatalf("stopped markerless legacy error = %v", err)
	}
	if _, err := os.Stat(profileSentinelPath(dataDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stopped unapproved legacy wrote internal marker: %v", err)
	}
	if _, err := os.Stat(profileMetadataPath(account.StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stopped unapproved legacy wrote external marker: %v", err)
	}
}

func TestStoppedMarkerlessLegacyProfileConsumesBoundApproval(t *testing.T) {
	account := profileTestAccount(t, "work")
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyState := filepath.Join(dataDir, "existing-login-state")
	if err := os.WriteFile(legacyState, []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := createLegacyProfileApproval(account.StateDir, account.AccountID, testContainerExecutionEpoch(account.AccountID)); err != nil {
		t.Fatalf("create stopped legacy approval: %v", err)
	}

	config := validTestConfig(t)
	executor := &sequenceExecutor{results: []executorResult{
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, false, false)},
		{output: networkInspection(t, account.AccountID)},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, false, false)},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, false, false)},
		{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, dataDir, false, false)},
	}}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	configureProfileTestRuntime(runtime, account, dataDir)
	if err := runtime.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatalf("adopt approved stopped legacy profile: %v", err)
	}
	if _, err := os.Stat(legacyProfileApprovalPath(account.StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stopped legacy approval was not consumed: %v", err)
	}
	metadata, metadataExists, err := readWeComProfileMetadata(profileMetadataPath(account.StateDir))
	if err != nil || !metadataExists {
		t.Fatalf("approved stopped legacy metadata: exists=%v err=%v", metadataExists, err)
	}
	sentinel, sentinelExists, err := readWeComProfileSentinel(profileSentinelPath(dataDir))
	if err != nil || !sentinelExists {
		t.Fatalf("approved stopped legacy sentinel: exists=%v err=%v", sentinelExists, err)
	}
	if err := validateWeComProfileSentinel(sentinel, metadata); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(legacyState); err != nil || string(contents) != "preserve me" {
		t.Fatalf("approved stopped legacy state changed: contents=%q err=%v", contents, err)
	}
}

func TestLegacyOwnershipHelperIsNarrowlySandboxed(t *testing.T) {
	image := "registry.example/redroid@sha256:" + strings.Repeat("a", 64)
	dataDir := "/srv/wechatcopilot-state/accounts/account-1/wecom/android-data"
	args := legacyProfileOwnershipHelperArgs(image, dataDir, 1001, 1002, 41, 42)
	for _, pair := range [][2]string{
		{"--pull", "never"}, {"--network", "none"}, {"--cap-drop", "ALL"},
		{"--cap-add", "CHOWN"}, {"--security-opt", "no-new-privileges=true"},
		{"--user", "0:0"}, {"--entrypoint", "/system/bin/toybox"},
		{"--mount", "type=bind,src=" + dataDir + ",dst=/data"},
	} {
		if !containsArgumentPair(args, pair[0], pair[1]) {
			t.Fatalf("ownership helper lacks %q %q: %v", pair[0], pair[1], args)
		}
	}
	if !slicesContain(args, "--read-only") || !slicesContain(args, "--rm") {
		t.Fatalf("ownership helper lacks read-only/ephemeral policy: %v", args)
	}
	if slicesContain(args, "--privileged") || slicesContain(args, "--network=host") {
		t.Fatalf("ownership helper received unsafe privilege or networking: %v", args)
	}
	wantTail := []string{"wechatcopilot-ownership-helper", "41", "42", "1001", "1002"}
	if len(args) < len(wantTail) || strings.Join(args[len(args)-len(wantTail):], "\x00") != strings.Join(wantTail, "\x00") {
		t.Fatalf("ownership helper command tail = %v, want %v", args, wantTail)
	}
	joinedArgs := strings.Join(args, "\n")
	if !slicesContain(args, image) || !slicesContain(args, "sh") || !slicesContain(args, "-c") ||
		!strings.Contains(joinedArgs, "/system/bin/toybox stat -Lc %d:%i /data") ||
		!strings.Contains(joinedArgs, "exec /system/bin/toybox chown \"$3:$4\" /data") {
		t.Fatalf("ownership helper does not gate chown on the mounted /data inode: %v", args)
	}
}

type profileFileInfoWithStat struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (info profileFileInfoWithStat) Sys() any { return &info.stat }

func foreignOwnedProfileInfo(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("profile fixture has no syscall.Stat_t")
	}
	copy := *stat
	copy.Uid = uint32(os.Geteuid() + 1)
	copy.Gid = uint32(os.Getegid() + 1)
	return profileFileInfoWithStat{FileInfo: info, stat: copy}
}

func TestNonUIDLegacyOwnershipHelperRunsOnlyAfterCompleteLiveProof(t *testing.T) {
	account := profileTestAccount(t, "work")
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	foreignInfo := foreignOwnedProfileInfo(t, dataDir)
	device, inode, err := directoryIdentity(foreignInfo)
	if err != nil {
		t.Fatal(err)
	}
	config := validTestConfig(t)
	executor := &sequenceExecutor{results: []executorResult{
		{output: runtimeInspectionWithID(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false, "proved-container")},
		{output: []byte(fmt.Sprintf("%d:%d\n", device, inode))},
		{output: runtimeInspectionWithID(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false, "proved-container")},
		{output: runtimeInspectionWithID(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false, "proved-container")},
		{output: nil},
		{output: runtimeInspectionWithID(t, "wecom", account.AccountID, config.RedroidImage, dataDir, false, false, "proved-container")},
		{output: nil},
	}}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	configureProfileTestRuntime(runtime, account, dataDir)
	if _, err := runtime.verifyRunningContainerProfileFrame(
		context.Background(), account,
		weComProfileMetadata{AccountID: account.AccountID, DataDevice: device, DataInode: inode}, false, "",
	); err != nil {
		t.Fatalf("complete live proof before ownership helper: %v", err)
	}
	if _, err := runtime.prepareLegacyDataDirectoryForSentinel(
		context.Background(), account, foreignInfo, true,
	); err != nil {
		t.Fatalf("restricted ownership helper after proof: %v", err)
	}
	if len(executor.commands) != 7 {
		t.Fatalf("proof/helper commands = %d, want 7", len(executor.commands))
	}
	for index, command := range executor.commands[:3] {
		if slicesContain(command.args, "run") || slicesContain(command.args, "stop") {
			t.Fatalf("ownership mutation occurred before proof frame completed at command %d: %v", index, command.args)
		}
	}
	stopArgs := executor.commands[4].args
	if len(stopArgs) < 5 || stopArgs[0] != "container" || stopArgs[1] != "stop" || stopArgs[len(stopArgs)-1] != testContainerID("proved-container") {
		t.Fatalf("ownership migration did not stop the immutable proved container ID: %v", stopArgs)
	}
	helper := executor.commands[6].args
	if len(helper) < 3 || helper[0] != "container" || helper[1] != "run" || !slicesContain(helper, config.RedroidImage) {
		t.Fatalf("unexpected restricted ownership helper command: %v", helper)
	}
	for _, marker := range []string{profileSentinelPath(dataDir), profileMetadataPath(account.StateDir)} {
		if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ownership preparation wrote profile marker %q: %v", marker, err)
		}
	}
}

func TestNonUIDLegacyOwnershipHelperFailuresWriteNoMarkers(t *testing.T) {
	t.Run("container replacement after stop", func(t *testing.T) {
		account := profileTestAccount(t, "work")
		dataDir, err := accountDataDir(account.StateDir, account.AccountID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		foreignInfo := foreignOwnedProfileInfo(t, dataDir)
		config := validTestConfig(t)
		executor := &sequenceExecutor{results: []executorResult{
			{output: runtimeInspectionWithID(t, "wecom", account.AccountID, config.RedroidImage, dataDir, true, false, "container-before")},
			{output: nil},
			{output: runtimeInspectionWithID(t, "wecom", account.AccountID, config.RedroidImage, dataDir, false, false, "container-after")},
		}}
		runtime, err := NewRuntime(config, executor)
		if err != nil {
			t.Fatal(err)
		}
		configureProfileTestRuntime(runtime, account, dataDir)
		if _, err := runtime.prepareLegacyDataDirectoryForSentinel(context.Background(), account, foreignInfo, true); err == nil || !strings.Contains(err.Error(), "changed") {
			t.Fatalf("ownership helper container replacement error = %v", err)
		}
		for _, command := range executor.commands {
			if len(command.args) >= 2 && command.args[0] == "container" && command.args[1] == "run" {
				t.Fatalf("container replacement reached ownership helper: %v", command.args)
			}
		}
	})

	t.Run("mounted inode mismatch", func(t *testing.T) {
		account := profileTestAccount(t, "work")
		dataDir, err := accountDataDir(account.StateDir, account.AccountID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		foreignInfo := foreignOwnedProfileInfo(t, dataDir)
		executor := &sequenceExecutor{results: []executorResult{{err: errors.New("exit status 72")}}}
		runtime := profileTestRuntime(t, account, executor)
		if _, err := runtime.prepareLegacyDataDirectoryForSentinel(context.Background(), account, foreignInfo, false); err == nil || !strings.Contains(err.Error(), "ownership helper") {
			t.Fatalf("ownership helper mounted-inode mismatch = %v", err)
		}
		if len(executor.commands) != 1 {
			t.Fatalf("mounted-inode mismatch helper commands = %d", len(executor.commands))
		}
		device, inode, err := directoryIdentity(foreignInfo)
		if err != nil {
			t.Fatal(err)
		}
		args := executor.commands[0].args
		wantTail := []string{
			"wechatcopilot-ownership-helper",
			strconv.FormatUint(device, 10), strconv.FormatUint(inode, 10),
			strconv.Itoa(os.Geteuid()), strconv.Itoa(os.Getegid()),
		}
		if len(args) < len(wantTail) || !reflect.DeepEqual(args[len(args)-len(wantTail):], wantTail) {
			t.Fatalf("ownership helper did not receive approved mounted inode: %v", args)
		}
		for _, marker := range []string{profileSentinelPath(dataDir), profileMetadataPath(account.StateDir)} {
			if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed helper wrote marker %q: %v", marker, err)
			}
		}
	})

	t.Run("canonical inode exchange", func(t *testing.T) {
		account := profileTestAccount(t, "work")
		dataDir, err := accountDataDir(account.StateDir, account.AccountID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		foreignInfo := foreignOwnedProfileInfo(t, dataDir)
		original := dataDir + ".original"
		executor := functionExecutor(func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if len(args) < 2 || args[0] != "container" || args[1] != "run" {
				return nil, errors.New("unexpected command before ownership helper")
			}
			if err := os.Rename(dataDir, original); err != nil {
				return nil, err
			}
			if err := os.Mkdir(dataDir, 0o700); err != nil {
				return nil, err
			}
			return nil, nil
		})
		runtime := profileTestRuntime(t, account, executor)
		if _, err := runtime.prepareLegacyDataDirectoryForSentinel(context.Background(), account, foreignInfo, false); err == nil || !strings.Contains(err.Error(), "after ownership migration") {
			t.Fatalf("ownership inode exchange error = %v", err)
		}
		for _, marker := range []string{profileSentinelPath(original), profileSentinelPath(dataDir), profileMetadataPath(account.StateDir)} {
			if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("exchanged helper wrote marker %q: %v", marker, err)
			}
		}
	})
}

func TestLegacyOwnershipValidationRejectsUnchangedForeignUID(t *testing.T) {
	stat := &syscall.Stat_t{Uid: uint32(os.Geteuid() + 1), Gid: uint32(os.Getegid() + 1)}
	if err := validateLegacyDataOwnership(stat, uint32(os.Geteuid()), uint32(os.Getegid())); err == nil {
		t.Fatal("unchanged foreign ownership passed helper validation")
	}
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestPersistentProfileRejectsUnverifiableLegacyData(t *testing.T) {
	account := profileTestAccount(t, "work")
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "unbound-state"), []byte("present"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &sequenceExecutor{results: []executorResult{
		{err: errors.New("container not found")},
		{output: nil},
	}}
	runtime := profileTestRuntime(t, account, executor)
	err = runtime.preparePersistentProfile(context.Background(), account)
	if err == nil || !strings.Contains(err.Error(), "refusing automatic adoption") {
		t.Fatalf("unverifiable legacy data error = %v", err)
	}
	if _, exists, err := readWeComProfileMetadata(profileMetadataPath(account.StateDir)); err != nil || exists {
		t.Fatalf("unverified data was marked as adopted: exists=%v err=%v", exists, err)
	}
}

func TestPersistentProfileRecoversEmptyDirectoryCreatedBeforeSentinel(t *testing.T) {
	account := profileTestAccount(t, "work")
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runtime := profileTestRuntime(t, account, &sequenceExecutor{results: []executorResult{
		{err: errors.New("container not found")},
		{output: nil},
	}})
	if err := runtime.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatalf("recover empty interrupted profile initialization: %v", err)
	}
	metadata, metadataExists, err := readWeComProfileMetadata(profileMetadataPath(account.StateDir))
	if err != nil || !metadataExists {
		t.Fatalf("recovered external metadata: exists=%v err=%v", metadataExists, err)
	}
	sentinel, sentinelExists, err := readWeComProfileSentinel(profileSentinelPath(dataDir))
	if err != nil || !sentinelExists {
		t.Fatalf("recovered internal sentinel: exists=%v err=%v", sentinelExists, err)
	}
	if err := validateWeComProfileSentinel(sentinel, metadata); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dataDir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("recovered data directory mode: info=%v err=%v", info, err)
	}
}

func TestPersistentProfileRejectsMissingOrExchangedData(t *testing.T) {
	for _, test := range []struct {
		name    string
		replace func(*testing.T, string)
		want    string
	}{
		{
			name: "missing",
			replace: func(t *testing.T, dataDir string) {
				if err := os.RemoveAll(dataDir); err != nil {
					t.Fatal(err)
				}
			},
			want: "missing",
		},
		{
			name: "exchanged",
			replace: func(t *testing.T, dataDir string) {
				original := dataDir + ".original"
				if err := os.Rename(dataDir, original); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dataDir, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: "replaced or exchanged",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := profileTestAccount(t, "work")
			executor := &sequenceExecutor{results: []executorResult{
				{err: errors.New("container not found")},
				{output: nil},
			}}
			runtime := profileTestRuntime(t, account, executor)
			if err := runtime.preparePersistentProfile(context.Background(), account); err != nil {
				t.Fatal(err)
			}
			test.replace(t, runtime.dataDir)
			restarted := profileTestRuntime(t, account, &sequenceExecutor{})
			err := restarted.preparePersistentProfile(context.Background(), account)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("damaged profile error = %v, want %q", err, test.want)
			}
			if _, exists, statErr := inspectRealDirectory(restarted.dataDir); test.name == "missing" && (statErr != nil || exists) {
				t.Fatalf("missing data directory was recreated: exists=%v err=%v", exists, statErr)
			}
		})
	}
}

func TestPersistentProfileRejectsSymlinkedData(t *testing.T) {
	account := profileTestAccount(t, "work")
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dataDir), 0o700); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.Symlink(other, dataDir); err != nil {
		t.Fatal(err)
	}
	runtime := profileTestRuntime(t, account, &sequenceExecutor{})
	err = runtime.preparePersistentProfile(context.Background(), account)
	if err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Fatalf("symlinked data error = %v", err)
	}
	if _, err := os.Stat(profileMetadataPath(account.StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlinked data received metadata: %v", err)
	}
}

func TestPersistentProfileRejectsSymlinkedMetadataWithoutCreatingData(t *testing.T) {
	account := profileTestAccount(t, "work")
	metadataPath := profileMetadataPath(account.StateDir)
	if err := os.Symlink(filepath.Join(t.TempDir(), "metadata.json"), metadataPath); err != nil {
		t.Fatal(err)
	}
	runtime := profileTestRuntime(t, account, &sequenceExecutor{})
	err := runtime.preparePersistentProfile(context.Background(), account)
	if err == nil || !strings.Contains(err.Error(), "without following symlinks") {
		t.Fatalf("symlinked metadata error = %v", err)
	}
	if _, exists, statErr := inspectRealDirectory(runtime.dataDir); statErr != nil || exists {
		t.Fatalf("symlinked metadata caused data creation: exists=%v err=%v", exists, statErr)
	}
}

func TestPersistentProfileRejectsMetadataBoundToAnotherAccount(t *testing.T) {
	account := profileTestAccount(t, "work")
	runtime := profileTestRuntime(t, account, &sequenceExecutor{})
	if err := os.Mkdir(runtime.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dataInfo, err := os.Lstat(runtime.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := newWeComProfileMetadata("other", runtime.dataDir, dataInfo)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeNewWeComProfileMetadata(profileMetadataPath(account.StateDir), metadata); err != nil {
		t.Fatal(err)
	}
	if err := runtime.preparePersistentProfile(context.Background(), account); err == nil || !strings.Contains(err.Error(), "another account") {
		t.Fatalf("wrong-account metadata error = %v", err)
	}
}

func TestPersistentProfileRejectsInPlaceClearedDataDirectory(t *testing.T) {
	account := profileTestAccount(t, "work")
	runtime := profileTestRuntime(t, account, &sequenceExecutor{results: []executorResult{
		{err: errors.New("container not found")},
		{output: nil},
	}})
	if err := runtime.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(runtime.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(runtime.dataDir, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
	restarted := profileTestRuntime(t, account, &sequenceExecutor{})
	err = restarted.preparePersistentProfile(context.Background(), account)
	if err == nil || !strings.Contains(err.Error(), "sentinel is missing") {
		t.Fatalf("cleared directory error = %v", err)
	}
}

func TestPersistentProfileRejectsMismatchedOrLinkedInternalSentinel(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
		want   string
	}{
		{
			name: "mismatched identity",
			mutate: func(t *testing.T, path string) {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := writeNewWeComProfileSentinel(path, weComProfileSentinel{
					SchemaVersion: weComProfileSentinelSchema,
					AccountID:     "work",
					ProfileID:     strings.Repeat("a", 32),
					CreatedAt:     time.Now().UTC(),
				}); err != nil {
					t.Fatal(err)
				}
			},
			want: "does not match",
		},
		{
			name: "symlink",
			mutate: func(t *testing.T, path string) {
				contents, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "sentinel.json")
				if err := os.WriteFile(target, contents, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
			want: "without following symlinks",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := profileTestAccount(t, "work")
			runtime := profileTestRuntime(t, account, &sequenceExecutor{results: []executorResult{
				{err: errors.New("container not found")},
				{output: nil},
			}})
			if err := runtime.preparePersistentProfile(context.Background(), account); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, profileSentinelPath(runtime.dataDir))
			restarted := profileTestRuntime(t, account, &sequenceExecutor{})
			err := restarted.preparePersistentProfile(context.Background(), account)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("tampered sentinel error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestPersistentProfileRecoversInterruptedEmptyInitialization(t *testing.T) {
	account := profileTestAccount(t, "work")
	runtime := profileTestRuntime(t, account, &sequenceExecutor{results: []executorResult{
		{err: errors.New("container not found")},
		{output: nil},
	}})
	if err := os.Mkdir(runtime.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dataInfo, err := os.Lstat(runtime.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := newWeComProfileMetadata(account.AccountID, runtime.dataDir, dataInfo)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeNewWeComProfileSentinel(profileSentinelPath(runtime.dataDir), sentinelForMetadata(metadata)); err != nil {
		t.Fatal(err)
	}
	if err := runtime.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	recovered, exists, err := readWeComProfileMetadata(profileMetadataPath(account.StateDir))
	if err != nil || !exists {
		t.Fatalf("recovered metadata: exists=%v err=%v", exists, err)
	}
	if recovered.ProfileID != metadata.ProfileID {
		t.Fatalf("interrupted initialization identity changed: before=%s after=%s", metadata.ProfileID, recovered.ProfileID)
	}
}

func TestPersistentProfileRejectsForeignLegacyContainer(t *testing.T) {
	account := profileTestAccount(t, "work")
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := validTestConfig(t)
	executor := &sequenceExecutor{results: []executorResult{{
		output: runtimeInspection(t, "wecom", "other", config.RedroidImage, dataDir, false, false),
	}}}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	configureProfileTestRuntime(runtime, account, dataDir)
	if err := runtime.preparePersistentProfile(context.Background(), account); err == nil {
		t.Fatal("foreign legacy container was adopted")
	}
	if _, err := os.Stat(profileMetadataPath(account.StateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign container received metadata: %v", err)
	}
}

func TestAccountStateDirectoryMustMatchAccountID(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "different")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := validateAccountStateDir(stateDir, "work"); err == nil {
		t.Fatal("mismatched account state directory was accepted")
	}
}

func TestAccountStateDirectoryRejectsAnyAncestorSymlink(t *testing.T) {
	root := t.TempDir()
	realRoot := filepath.Join(root, "real")
	stateDir := filepath.Join(realRoot, "accounts", "work")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(root, "linked")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}
	throughSymlink := filepath.Join(linkedRoot, "accounts", "work")
	if _, err := validateAccountStateDir(throughSymlink, "work"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("ancestor-symlink state directory error = %v", err)
	}
	metadata := weComProfileMetadata{
		SchemaVersion: weComProfileSchemaVersion,
		AccountID:     "work",
		ProfileID:     strings.Repeat("a", 32),
		CreatedAt:     time.Now().UTC(),
	}
	unsafeMetadataPath := profileMetadataPath(throughSymlink)
	if err := writeNewWeComProfileMetadata(unsafeMetadataPath, metadata); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("ancestor-symlink metadata write error = %v", err)
	}
	if _, err := os.Stat(profileMetadataPath(stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe metadata write reached symlink target: %v", err)
	}
}

func TestPersistentProfileRevalidationRejectsDataDirectoryExchange(t *testing.T) {
	account := profileTestAccount(t, "work")
	runtime := profileTestRuntime(t, account, &sequenceExecutor{results: []executorResult{
		{err: errors.New("container not found")},
		{output: nil},
	}})
	if err := runtime.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	sentinelPath := profileSentinelPath(runtime.dataDir)
	sentinel, err := os.ReadFile(sentinelPath)
	if err != nil {
		t.Fatal(err)
	}
	original := runtime.dataDir + ".original"
	if err := os.Rename(runtime.dataDir, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(runtime.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinelPath, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.verifyPersistentProfileBinding(account); err == nil || !strings.Contains(err.Error(), "replaced or exchanged") {
		t.Fatalf("exchanged data revalidation error = %v", err)
	}
}

func TestPinnedProfileValidationNeverReadsSentinelFromExchangedPath(t *testing.T) {
	account := profileTestAccount(t, "work")
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dataInfo, err := os.Stat(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := newWeComProfileMetadata(account.AccountID, dataDir, dataInfo)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := openProfileDirectoryWithoutSymlinks(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.close()

	original := dataDir + ".original"
	replacement := dataDir + ".replacement"
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeNewWeComProfileSentinel(
		profileSentinelPath(replacement), sentinelForMetadata(metadata),
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dataDir, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, dataDir); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := readWeComProfileSentinel(profileSentinelPath(dataDir)); err != nil || !exists {
		t.Fatalf("replacement path did not contain the attack sentinel: exists=%v err=%v", exists, err)
	}
	if err := verifyBoundWeComProfileAt(account.AccountID, dataDir, metadata, pinned); err == nil || !strings.Contains(err.Error(), "sentinel is missing") {
		t.Fatalf("pinned validation accepted replacement sentinel: %v", err)
	}
}

func TestLegacyProfilePublicationRollsBackMarkersOnCanonicalExchange(t *testing.T) {
	for _, phase := range []string{"after sentinel", "after metadata"} {
		t.Run(phase, func(t *testing.T) {
			account := profileTestAccount(t, "work")
			dataDir, err := accountDataDir(account.StateDir, account.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				t.Fatal(err)
			}
			legacyState := filepath.Join(dataDir, "existing-login-state")
			if err := os.WriteFile(legacyState, []byte("preserve me"), 0o600); err != nil {
				t.Fatal(err)
			}
			pinned, err := openProfileDirectoryWithoutSymlinks(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer pinned.close()
			metadata, err := newWeComProfileMetadata(account.AccountID, dataDir, pinned.info)
			if err != nil {
				t.Fatal(err)
			}
			original := dataDir + ".authorized"
			exchange := func() {
				if err := os.Rename(dataDir, original); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dataDir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			hooks := &profilePublicationHooks{}
			if phase == "after sentinel" {
				hooks.afterSentinel = exchange
			} else {
				hooks.afterMetadata = exchange
			}
			err = publishLegacyWeComProfileBinding(
				account.AccountID, dataDir, profileMetadataPath(account.StateDir),
				metadata, pinned, true, hooks,
			)
			if err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("canonical exchange publication error = %v", err)
			}
			for _, marker := range []string{
				profileMetadataPath(account.StateDir),
				profileSentinelPath(original),
				profileSentinelPath(dataDir),
			} {
				if _, statErr := os.Lstat(marker); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("failed publication left durable marker %q: %v", marker, statErr)
				}
			}
			if contents, readErr := os.ReadFile(filepath.Join(original, "existing-login-state")); readErr != nil || string(contents) != "preserve me" {
				t.Fatalf("rollback changed legacy state: contents=%q err=%v", contents, readErr)
			}
		})
	}
}

func TestInitialProfilePublicationRollsBackMarkersOnCanonicalExchange(t *testing.T) {
	for _, phase := range []string{"after sentinel", "after metadata"} {
		t.Run(phase, func(t *testing.T) {
			account := profileTestAccount(t, "work")
			dataDir, err := accountDataDir(account.StateDir, account.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				t.Fatal(err)
			}
			pinned, err := openProfileDirectoryWithoutSymlinks(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer pinned.close()
			original := dataDir + ".initial"
			exchange := func() {
				if err := os.Rename(dataDir, original); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dataDir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			hooks := &profilePublicationHooks{}
			if phase == "after sentinel" {
				hooks.afterSentinel = exchange
			} else {
				hooks.afterMetadata = exchange
			}
			err = initializeWeComProfileDocumentsAt(
				account.AccountID, dataDir, profileMetadataPath(account.StateDir), pinned, hooks,
			)
			if err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("initial canonical exchange error = %v", err)
			}
			for _, marker := range []string{
				profileMetadataPath(account.StateDir),
				profileSentinelPath(original),
				profileSentinelPath(dataDir),
			} {
				if _, statErr := os.Lstat(marker); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("failed initial publication left marker %q: %v", marker, statErr)
				}
			}
		})
	}
}

func TestPinnedDirectoryListingIgnoresCanonicalPathExchange(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "android-data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "existing-login-state"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	pinned, err := openProfileDirectoryWithoutSymlinks(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.close()
	original := dataDir + ".original"
	if err := os.Rename(dataDir, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := readPinnedProfileDirectoryAt(pinned)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "existing-login-state" {
		t.Fatalf("pinned listing followed exchanged canonical path: %#v", entries)
	}
}

func TestRunningContainerProfileProofRequiresMatchingLiveSentinel(t *testing.T) {
	account := profileTestAccount(t, "work")
	initializer := profileTestRuntime(t, account, &sequenceExecutor{results: []executorResult{
		{err: errors.New("container not found")},
		{output: nil},
	}})
	if err := initializer.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	metadata, exists, err := readWeComProfileMetadata(profileMetadataPath(account.StateDir))
	if err != nil || !exists {
		t.Fatalf("read initialized metadata: exists=%v err=%v", exists, err)
	}
	liveInfo, err := os.Stat(initializer.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	liveDevice, liveInode, err := directoryIdentity(liveInfo)
	if err != nil {
		t.Fatal(err)
	}
	identity := []byte(fmt.Sprintf("%d:%d\n", liveDevice, liveInode))
	validSentinel, err := os.ReadFile(profileSentinelPath(initializer.dataDir))
	if err != nil {
		t.Fatal(err)
	}
	config := validTestConfig(t)

	for _, test := range []struct {
		name           string
		sentinelOutput []byte
		sentinelError  error
		wantError      bool
	}{
		{name: "matching", sentinelOutput: validSentinel},
		{name: "missing", sentinelError: errors.New("container sentinel missing"), wantError: true},
		{
			name: "wrong profile",
			sentinelOutput: func() []byte {
				wrong := sentinelForMetadata(metadata)
				wrong.ProfileID = strings.Repeat("f", 32)
				if wrong.ProfileID == metadata.ProfileID {
					wrong.ProfileID = strings.Repeat("e", 32)
				}
				contents, marshalErr := json.Marshal(wrong)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				return contents
			}(),
			wantError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor := &sequenceExecutor{results: []executorResult{
				{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, initializer.dataDir, true, false)},
				{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, initializer.dataDir, true, false)},
				{output: identity},
				{output: test.sentinelOutput, err: test.sentinelError},
				{output: runtimeInspection(t, "wecom", account.AccountID, config.RedroidImage, initializer.dataDir, true, false)},
			}}
			runtime, err := NewRuntime(config, executor)
			if err != nil {
				t.Fatal(err)
			}
			configureProfileTestRuntime(runtime, account, initializer.dataDir)
			exists, running, err := runtime.inspectContainer(context.Background(), account.AccountID, initializer.dataDir)
			if err != nil || !exists || !running {
				t.Fatalf("same-source running container inspection: exists=%v running=%v err=%v", exists, running, err)
			}
			proofErr := runtime.verifyRunningContainerProfile(context.Background(), account)
			if test.wantError && proofErr == nil {
				t.Fatal("same-source container with an invalid live sentinel passed profile proof")
			}
			if !test.wantError && proofErr != nil {
				t.Fatalf("matching live profile proof failed: %v", proofErr)
			}
			if executor.calls != len(executor.results) {
				t.Fatalf("live profile proof used %d executor calls, want %d", executor.calls, len(executor.results))
			}
			for _, commandIndex := range []int{2, 3} {
				args := executor.commands[commandIndex].args
				wantID := testContainerID("container-" + account.AccountID)
				if len(args) < 7 || args[0] != "container" || args[1] != "exec" ||
					args[2] != "--user" || args[3] != "0:0" || args[4] != wantID ||
					args[5] != "/system/bin/toybox" {
					t.Fatalf("proof command did not target immutable S0 ID with fixed toybox: %v", args)
				}
			}
		})
	}
}

func TestRunningContainerProfileProofRejectsContainerReplacementAcrossFrame(t *testing.T) {
	account := profileTestAccount(t, "work")
	initializer := profileTestRuntime(t, account, &sequenceExecutor{results: []executorResult{
		{err: errors.New("container not found")},
		{output: nil},
	}})
	if err := initializer.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	_, exists, err := readWeComProfileMetadata(profileMetadataPath(account.StateDir))
	if err != nil || !exists {
		t.Fatalf("read initialized metadata: exists=%v err=%v", exists, err)
	}
	sentinel, err := os.ReadFile(profileSentinelPath(initializer.dataDir))
	if err != nil {
		t.Fatal(err)
	}
	liveInfo, err := os.Stat(initializer.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	liveDevice, liveInode, err := directoryIdentity(liveInfo)
	if err != nil {
		t.Fatal(err)
	}
	config := validTestConfig(t)
	executor := &sequenceExecutor{results: []executorResult{
		{output: runtimeInspectionWithID(t, "wecom", account.AccountID, config.RedroidImage, initializer.dataDir, true, false, "container-before")},
		{output: []byte(fmt.Sprintf("%d:%d\n", liveDevice, liveInode))},
		{output: sentinel},
		{output: runtimeInspectionWithID(t, "wecom", account.AccountID, config.RedroidImage, initializer.dataDir, true, false, "container-after")},
	}}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	configureProfileTestRuntime(runtime, account, initializer.dataDir)
	proofErr := runtime.verifyRunningContainerProfile(context.Background(), account)
	if proofErr == nil || !strings.Contains(proofErr.Error(), "changed during live profile proof") {
		t.Fatalf("container replacement proof error = %v", proofErr)
	}
	if executor.calls != len(executor.results) {
		t.Fatalf("container replacement proof used %d executor calls, want %d", executor.calls, len(executor.results))
	}
}

func TestPinnedRunningProfileProofIgnoresReusedMutableContainerName(t *testing.T) {
	account := profileTestAccount(t, "work")
	initializer := profileTestRuntime(t, account, &sequenceExecutor{results: []executorResult{
		{err: errors.New("container not found")},
		{output: nil},
	}})
	if err := initializer.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	sentinel, err := os.ReadFile(profileSentinelPath(initializer.dataDir))
	if err != nil {
		t.Fatal(err)
	}
	liveInfo, err := os.Stat(initializer.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	liveDevice, liveInode, err := directoryIdentity(liveInfo)
	if err != nil {
		t.Fatal(err)
	}
	config := validTestConfig(t)
	containerA := testContainerID("pinned-profile-original")
	containerB := testContainerID("pinned-profile-name-replacement")
	originalName := containerName(account.AccountID)
	executor := functionExecutor(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch {
		case len(args) == 3 && args[0] == "container" && args[1] == "inspect" && args[2] == containerA:
			return runtimeInspectionWithIDAndName(
				t, "renamed-original", "wecom", account.AccountID, config.RedroidImage,
				initializer.dataDir, true, false, containerA,
			), nil
		case len(args) == 3 && args[0] == "container" && args[1] == "inspect" && args[2] == originalName:
			return runtimeInspectionWithIDAndName(
				t, originalName, "wecom", account.AccountID, config.RedroidImage,
				initializer.dataDir, true, false, containerB,
			), nil
		case len(args) >= 7 && args[0] == "container" && args[1] == "exec":
			if args[4] != containerA {
				t.Fatalf("live profile proof targeted replacement container: %v", args)
			}
			if args[6] == "stat" {
				return []byte(fmt.Sprintf("%d:%d\n", liveDevice, liveInode)), nil
			}
			if args[6] == "head" {
				return sentinel, nil
			}
			return nil, fmt.Errorf("unexpected pinned proof exec: %v", args)
		default:
			return nil, fmt.Errorf("unexpected pinned proof command: %v", args)
		}
	})
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	configureProfileTestRuntime(runtime, account, initializer.dataDir)

	resolved, err := runtime.resolvePinnedRunningContainerProfile(context.Background(), account, containerA)
	if err != nil {
		t.Fatalf("prove renamed pinned profile while name is reused: %v", err)
	}
	if resolved != containerA {
		t.Fatalf("resolved container = %s, want original %s (replacement %s)", resolved, containerA, containerB)
	}
}

func TestContainerExecReadinessRetriesAgainstOneImmutableContainer(t *testing.T) {
	account := profileTestAccount(t, "work")
	initializer := profileTestRuntime(t, account, &sequenceExecutor{results: []executorResult{
		{err: errors.New("container not found")},
		{output: nil},
	}})
	if err := initializer.preparePersistentProfile(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	config := validTestConfig(t)
	containerID := testContainerID("container-" + account.AccountID)
	running := runtimeInspectionWithID(
		t, "wecom", account.AccountID, config.RedroidImage, initializer.dataDir, true, false, containerID,
	)
	executor := &sequenceExecutor{results: []executorResult{
		{output: running},
		{err: errors.New("container exec is not ready")},
		{output: running},
		{output: nil},
		{output: running},
	}}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	configureProfileTestRuntime(runtime, account, initializer.dataDir)
	if err := runtime.waitForContainerExecReady(context.Background(), account, containerID); err != nil {
		t.Fatalf("wait for transient container exec readiness: %v", err)
	}
	if executor.calls != len(executor.results) {
		t.Fatalf("exec readiness used %d calls, want %d", executor.calls, len(executor.results))
	}
	for _, commandIndex := range []int{1, 3} {
		args := executor.commands[commandIndex].args
		if len(args) != 7 || args[0] != "container" || args[1] != "exec" ||
			args[2] != "--user" || args[3] != "0:0" || args[4] != containerID ||
			args[5] != "/system/bin/toybox" || args[6] != "true" {
			t.Fatalf("readiness probe was not immutable-ID-bound toybox true: %v", args)
		}
	}
}

func profileTestAccount(t *testing.T, accountID string) core.AccountRuntime {
	t.Helper()
	return core.AccountRuntime{AccountID: accountID, Alias: accountID, StateDir: testAccountStateDir(t, accountID)}
}

func profileTestRuntime(t *testing.T, account core.AccountRuntime, executor Executor) *Runtime {
	t.Helper()
	config := validTestConfig(t)
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureManagedDirectory(filepath.Dir(dataDir)); err != nil {
		t.Fatal(err)
	}
	configureProfileTestRuntime(runtime, account, dataDir)
	return runtime
}

func configureProfileTestRuntime(runtime *Runtime, account core.AccountRuntime, dataDir string) {
	runtime.account = account
	runtime.dataDir = dataDir
	runtime.containerName = containerName(account.AccountID)
	runtime.networkName = networkName(account.AccountID)
}
