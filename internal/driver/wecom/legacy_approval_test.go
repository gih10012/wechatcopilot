package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestStoppedLegacyProfileApprovalRequiresExactStoppedContainerFrame(t *testing.T) {
	for _, test := range []struct {
		name    string
		results func(*testing.T, Config, string) []executorResult
		want    string
	}{
		{
			name: "running",
			results: func(t *testing.T, config Config, dataDir string) []executorResult {
				return []executorResult{{output: runtimeInspection(t, "wecom", "work", config.RedroidImage, dataDir, true, false)}}
			},
			want: "still running",
		},
		{
			name: "container replaced after publication",
			results: func(t *testing.T, config Config, dataDir string) []executorResult {
				return []executorResult{
					{output: runtimeInspectionWithID(t, "wecom", "work", config.RedroidImage, dataDir, false, false, "container-before")},
					{output: networkInspection(t, "work")},
					{output: runtimeInspectionWithID(t, "wecom", "work", config.RedroidImage, dataDir, false, false, "container-after")},
				}
			},
			want: "changed during approval",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateDir, dataDir := markerlessLegacyProfileFixture(t, "work")
			config := validTestConfig(t)
			executor := &sequenceExecutor{results: test.results(t, config, dataDir)}
			err := CreateStoppedLegacyProfileApproval(
				context.Background(), stateDir, "work",
				LegacyProfileApprovalConfig{
					DockerBinary: config.DockerBinary, RedroidImage: config.RedroidImage, Executor: executor,
				},
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("stopped-container approval error = %v, want %q", err, test.want)
			}
			assertNoLegacyProfileApproval(t, stateDir)
		})
	}
}

func TestLegacyProfileApprovalRollsBackAfterPostPublicationDataExchange(t *testing.T) {
	stateDir, dataDir := markerlessLegacyProfileFixture(t, "work")
	original := dataDir + ".approved"
	err := createLegacyProfileApprovalWithHook(stateDir, "work", testContainerExecutionEpoch("work"), func() {
		if err := os.Rename(dataDir, original); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "changed while approval was recorded") {
		t.Fatalf("post-publication exchange error = %v", err)
	}
	assertNoLegacyProfileApproval(t, stateDir)
}

func TestLegacyProfileApprovalAcceptsValidInterruptedSentinel(t *testing.T) {
	stateDir, dataDir := markerlessLegacyProfileFixture(t, "work")
	dataInfo, err := os.Stat(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := newWeComProfileMetadata("work", dataDir, dataInfo)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeNewWeComProfileSentinel(profileSentinelPath(dataDir), sentinelForMetadata(metadata)); err != nil {
		t.Fatal(err)
	}
	if err := createLegacyProfileApproval(stateDir, "work", testContainerExecutionEpoch("work")); err != nil {
		t.Fatalf("approve interrupted sentinel publication: %v", err)
	}
	if err := consumeLegacyProfileApprovalForData(stateDir, "work", dataDir, testContainerExecutionEpoch("work")); err != nil {
		t.Fatalf("consume interrupted sentinel approval: %v", err)
	}
	stored, exists, err := readWeComProfileSentinel(profileSentinelPath(dataDir))
	if err != nil || !exists || stored.ProfileID != metadata.ProfileID {
		t.Fatalf("approval changed interrupted sentinel: exists=%v sentinel=%#v err=%v", exists, stored, err)
	}
}

func TestLegacyProfileApprovalCreateAndConsume(t *testing.T) {
	stateDir, dataDir := markerlessLegacyProfileFixture(t, "work")
	if err := createLegacyProfileApproval(stateDir, "work", testContainerExecutionEpoch("work")); err != nil {
		t.Fatalf("create approval: %v", err)
	}
	approvalPath := legacyProfileApprovalPath(stateDir)
	info, err := os.Lstat(approvalPath)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || stat.Nlink != 1 {
		t.Fatalf("approval identity/mode = %#v stat=%#v", info, stat)
	}
	handle, exists, err := openLegacyProfileApproval(stateDir)
	if err != nil || !exists {
		t.Fatalf("read approval: exists=%v err=%v", exists, err)
	}
	if handle.approval.SchemaVersion != legacyProfileApprovalSchemaVersion ||
		handle.approval.AccountID != "work" || handle.approval.CreatedAt.IsZero() ||
		handle.approval.DataPath != filepath.Clean(dataDir) || handle.approval.DataDevice != 0 {
		t.Fatalf("approval = %#v", handle.approval)
	}
	if err := handle.close(); err != nil {
		t.Fatal(err)
	}

	if err := consumeLegacyProfileApprovalForData(stateDir, "work", dataDir, testContainerExecutionEpoch("work")); err != nil {
		t.Fatalf("consume approval: %v", err)
	}
	if _, err := os.Lstat(approvalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("consumed approval still exists: %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(dataDir, "existing-login-state")); err != nil || string(contents) != "preserve me" {
		t.Fatalf("approval changed legacy data: contents=%q err=%v", contents, err)
	}
	for _, forbidden := range []string{profileMetadataPath(stateDir), profileSentinelPath(dataDir)} {
		if _, err := os.Lstat(forbidden); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("approval created a profile marker %q: %v", filepath.Base(forbidden), err)
		}
	}
	if err := consumeLegacyProfileApprovalForData(stateDir, "work", dataDir, testContainerExecutionEpoch("work")); err == nil || !strings.Contains(err.Error(), "approval is missing") {
		t.Fatalf("replayed approval error = %v", err)
	}
}

func TestLegacyProfileApprovalConsumesSchemaTwoAfterDeviceRenumbering(t *testing.T) {
	stateDir, dataDir := markerlessLegacyProfileFixture(t, "work")
	epoch := testContainerExecutionEpoch("work")
	if err := createLegacyProfileApproval(stateDir, "work", epoch); err != nil {
		t.Fatal(err)
	}
	handle, exists, err := openLegacyProfileApproval(stateDir)
	if err != nil || !exists {
		t.Fatalf("open current approval: exists=%v err=%v", exists, err)
	}
	previous := handle.approval
	if err := handle.close(); err != nil {
		t.Fatal(err)
	}
	dataInfo, err := os.Stat(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	device, _, err := directoryIdentity(dataInfo)
	if err != nil {
		t.Fatal(err)
	}
	previous.SchemaVersion = legacyProfileApprovalPreviousSchema
	previous.DataPath = ""
	previous.DataDevice = device + 1 // Simulate approval before mapper reconstruction.
	approvalPath := legacyProfileApprovalPath(stateDir)
	if err := os.Remove(approvalPath); err != nil {
		t.Fatal(err)
	}
	if err := writeNewProfileDocument(
		approvalPath, previous, ".wecom-legacy-profile-approval-*.tmp", "legacy WeCom profile approval",
	); err != nil {
		t.Fatal(err)
	}

	if err := consumeLegacyProfileApprovalForData(stateDir, "work", dataDir, epoch); err != nil {
		t.Fatalf("consume schema-2 approval after device renumbering: %v", err)
	}
	if _, err := os.Lstat(approvalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("consumed schema-2 approval still exists: %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(dataDir, "existing-login-state")); err != nil || string(contents) != "preserve me" {
		t.Fatalf("schema-2 compatibility changed profile contents: contents=%q err=%v", contents, err)
	}
}

func TestLegacyProfileApprovalRejectsDifferentCanonicalPath(t *testing.T) {
	stateDir, dataDir := markerlessLegacyProfileFixture(t, "work")
	epoch := testContainerExecutionEpoch("work")
	if err := createLegacyProfileApproval(stateDir, "work", epoch); err != nil {
		t.Fatal(err)
	}
	handle, exists, err := openLegacyProfileApproval(stateDir)
	if err != nil || !exists {
		t.Fatalf("open approval: exists=%v err=%v", exists, err)
	}
	wrongPath := handle.approval
	if err := handle.close(); err != nil {
		t.Fatal(err)
	}
	wrongPath.DataPath = filepath.Join(filepath.Dir(dataDir), "different-data")
	approvalPath := legacyProfileApprovalPath(stateDir)
	if err := os.Remove(approvalPath); err != nil {
		t.Fatal(err)
	}
	if err := writeNewProfileDocument(
		approvalPath, wrongPath, ".wecom-legacy-profile-approval-*.tmp", "legacy WeCom profile approval",
	); err != nil {
		t.Fatal(err)
	}

	err = consumeLegacyProfileApprovalForData(stateDir, "work", dataDir, epoch)
	if err == nil || !strings.Contains(err.Error(), "another canonical data path") {
		t.Fatalf("different-path approval error = %v", err)
	}
	if _, err := os.Lstat(approvalPath); err != nil {
		t.Fatalf("different-path approval was consumed: %v", err)
	}
}

func TestLegacyProfileApprovalRejectsUnknownSchemaWithoutConsumption(t *testing.T) {
	stateDir, dataDir := markerlessLegacyProfileFixture(t, "work")
	epoch := testContainerExecutionEpoch("work")
	if err := createLegacyProfileApproval(stateDir, "work", epoch); err != nil {
		t.Fatal(err)
	}
	handle, exists, err := openLegacyProfileApproval(stateDir)
	if err != nil || !exists {
		t.Fatalf("open approval: exists=%v err=%v", exists, err)
	}
	unknown := handle.approval
	if err := handle.close(); err != nil {
		t.Fatal(err)
	}
	unknown.SchemaVersion = legacyProfileApprovalSchemaVersion + 1
	approvalPath := legacyProfileApprovalPath(stateDir)
	if err := os.Remove(approvalPath); err != nil {
		t.Fatal(err)
	}
	if err := writeNewProfileDocument(
		approvalPath, unknown, ".wecom-legacy-profile-approval-*.tmp", "legacy WeCom profile approval",
	); err != nil {
		t.Fatal(err)
	}

	err = consumeLegacyProfileApprovalForData(stateDir, "work", dataDir, epoch)
	if err == nil || !strings.Contains(err.Error(), "unsupported legacy WeCom profile approval schema") {
		t.Fatalf("unknown approval schema error = %v", err)
	}
	if _, err := os.Lstat(approvalPath); err != nil {
		t.Fatalf("unknown approval schema was consumed: %v", err)
	}
}

func TestLegacyProfileApprovalRevokesChangedContainerExecutionEpoch(t *testing.T) {
	stateDir, dataDir := markerlessLegacyProfileFixture(t, "work")
	approvedEpoch := testContainerExecutionEpoch("work")
	if err := createLegacyProfileApproval(stateDir, "work", approvedEpoch); err != nil {
		t.Fatal(err)
	}
	currentEpoch := approvedEpoch
	currentEpoch.StartedAt = "2026-08-16T13:00:00Z"
	currentEpoch.FinishedAt = "2026-08-16T13:01:00Z"
	currentEpoch.RestartCount++

	err := consumeLegacyProfileApprovalForData(stateDir, "work", dataDir, currentEpoch)
	if err == nil || !strings.Contains(err.Error(), "ran or changed after approval") {
		t.Fatalf("changed container epoch error = %v", err)
	}
	if _, statErr := os.Lstat(legacyProfileApprovalPath(stateDir)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stale execution-epoch approval remained replayable: %v", statErr)
	}
	for _, marker := range []string{profileMetadataPath(stateDir), profileSentinelPath(dataDir)} {
		if _, statErr := os.Lstat(marker); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("stale approval wrote profile marker %q: %v", marker, statErr)
		}
	}
	if contents, readErr := os.ReadFile(filepath.Join(dataDir, "existing-login-state")); readErr != nil || string(contents) != "preserve me" {
		t.Fatalf("stale approval changed login state: contents=%q err=%v", contents, readErr)
	}
}

func TestLegacyProfileApprovalRejectsWrongAccount(t *testing.T) {
	stateDir, dataDir := markerlessLegacyProfileFixture(t, "work")
	if err := createLegacyProfileApproval(stateDir, "work", testContainerExecutionEpoch("work")); err != nil {
		t.Fatal(err)
	}
	if err := consumeLegacyProfileApprovalForData(stateDir, "other", dataDir, testContainerExecutionEpoch("work")); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("wrong account error = %v", err)
	}
	if _, err := os.Lstat(legacyProfileApprovalPath(stateDir)); err != nil {
		t.Fatalf("wrong account consumed approval: %v", err)
	}
}

func TestLegacyProfileApprovalRejectsMissingAndUnsafeApproval(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		stateDir, dataDir := markerlessLegacyProfileFixture(t, "work")
		err := consumeLegacyProfileApprovalForData(stateDir, "work", dataDir, testContainerExecutionEpoch("work"))
		if err == nil || !strings.Contains(err.Error(), "approval is missing") {
			t.Fatalf("missing approval error = %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		stateDir, dataDir := markerlessLegacyProfileFixture(t, "work")
		target := filepath.Join(t.TempDir(), "approval.json")
		contents, err := json.Marshal(legacyProfileApproval{
			SchemaVersion: 1, AccountID: "work", DataDevice: 1, DataInode: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, legacyProfileApprovalPath(stateDir)); err != nil {
			t.Fatal(err)
		}
		err = consumeLegacyProfileApprovalForData(stateDir, "work", dataDir, testContainerExecutionEpoch("work"))
		if err == nil || !strings.Contains(err.Error(), "without following symlinks") {
			t.Fatalf("symlink approval error = %v", err)
		}
		if _, err := os.Lstat(target); err != nil {
			t.Fatalf("symlink target changed: %v", err)
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		stateDir, dataDir := markerlessLegacyProfileFixture(t, "work")
		if err := createLegacyProfileApproval(stateDir, "work", testContainerExecutionEpoch("work")); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(legacyProfileApprovalPath(stateDir), filepath.Join(stateDir, "second-link")); err != nil {
			t.Fatal(err)
		}
		err := consumeLegacyProfileApprovalForData(stateDir, "work", dataDir, testContainerExecutionEpoch("work"))
		if err == nil || !strings.Contains(err.Error(), "single-link") {
			t.Fatalf("hardlinked approval error = %v", err)
		}
	})
}

func TestLegacyProfileApprovalRejectsMissingSymlinkedOrExchangedData(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		stateDir := testAccountStateDir(t, "work")
		if err := createLegacyProfileApproval(stateDir, "work", testContainerExecutionEpoch("work")); err == nil || !strings.Contains(err.Error(), "parent directory is missing") {
			t.Fatalf("missing data error = %v", err)
		}
		assertNoLegacyProfileApproval(t, stateDir)
	})

	t.Run("symlink", func(t *testing.T) {
		stateDir := testAccountStateDir(t, "work")
		parent := filepath.Join(stateDir, "wecom")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		realData := t.TempDir()
		if err := os.WriteFile(filepath.Join(realData, "state"), []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realData, filepath.Join(parent, "android-data")); err != nil {
			t.Fatal(err)
		}
		if err := createLegacyProfileApproval(stateDir, "work", testContainerExecutionEpoch("work")); err == nil || !strings.Contains(err.Error(), "real directory") {
			t.Fatalf("symlinked data error = %v", err)
		}
		assertNoLegacyProfileApproval(t, stateDir)
	})

	t.Run("exchanged", func(t *testing.T) {
		stateDir, dataDir := markerlessLegacyProfileFixture(t, "work")
		if err := createLegacyProfileApproval(stateDir, "work", testContainerExecutionEpoch("work")); err != nil {
			t.Fatal(err)
		}
		original := dataDir + ".approved"
		if err := os.Rename(dataDir, original); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "replacement-state"), []byte("other"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := consumeLegacyProfileApprovalForData(stateDir, "work", dataDir, testContainerExecutionEpoch("work"))
		if err == nil || !strings.Contains(err.Error(), "replaced or exchanged") {
			t.Fatalf("exchanged data error = %v", err)
		}
		if _, err := os.Lstat(legacyProfileApprovalPath(stateDir)); err != nil {
			t.Fatalf("exchanged data consumed approval: %v", err)
		}
	})
}

func markerlessLegacyProfileFixture(t *testing.T, accountID string) (string, string) {
	t.Helper()
	stateDir := testAccountStateDir(t, accountID)
	dataDir, err := accountDataDir(stateDir, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(dataDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "existing-login-state"), []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	return stateDir, dataDir
}

func assertNoLegacyProfileApproval(t *testing.T, stateDir string) {
	t.Helper()
	if _, err := os.Lstat(legacyProfileApprovalPath(stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected legacy profile approval: %v", err)
	}
}
