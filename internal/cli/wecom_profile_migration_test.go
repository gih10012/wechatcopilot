package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gih10012/wechatcopilot/internal/account"
	"github.com/gih10012/wechatcopilot/internal/api"
	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/gih10012/wechatcopilot/internal/daemon"
	"github.com/gih10012/wechatcopilot/internal/driver"
	"github.com/gih10012/wechatcopilot/internal/driver/wecom"
)

type legacyWeComCLIFixture struct {
	paths    config.Paths
	account  account.Account
	dataDir  string
	executor *legacyWeComCLIExecutor
}

type legacyWeComCLIExecutor struct {
	accountID string
	dataDir   string
	image     string
	running   bool
	calls     []executorCall
}

type executorCall struct {
	name string
	args []string
}

func (executor *legacyWeComCLIExecutor) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	executor.calls = append(executor.calls, executorCall{name: name, args: append([]string(nil), args...)})
	if len(args) >= 2 && args[0] == "container" && args[1] == "inspect" {
		return json.Marshal([]map[string]any{{
			"Id":           legacyWeComContainerID(executor.accountID),
			"Name":         "/" + legacyWeComContainerName(executor.accountID),
			"RestartCount": 0,
			"Config": map[string]any{
				"Image": executor.image, "Hostname": legacyWeComHostname(executor.accountID),
				"Labels": map[string]string{
					"dev.wechatcopilot.driver": "wecom", "dev.wechatcopilot.account": executor.accountID,
				},
			},
			"State": map[string]any{
				"Status":  map[bool]string{true: "running", false: "exited"}[executor.running],
				"Running": executor.running, "Paused": false, "Restarting": false,
				"ExitCode": 0, "StartedAt": "2026-08-16T12:00:00Z", "FinishedAt": "2026-08-16T12:01:00Z",
			},
			"HostConfig": map[string]any{
				"Privileged": true, "NetworkMode": legacyWeComNetworkName(executor.accountID),
				"PublishAllPorts": false, "PortBindings": map[string]any{"5555/tcp": nil},
			},
			"NetworkSettings": map[string]any{
				"Ports":    map[string]any{"5555/tcp": nil},
				"Networks": map[string]any{legacyWeComNetworkName(executor.accountID): map[string]any{}},
			},
			"Mounts": []map[string]any{{
				"Type": "bind", "Source": executor.dataDir, "Destination": "/data", "RW": true,
			}},
		}})
	}
	if len(args) >= 2 && args[0] == "network" && args[1] == "inspect" {
		return json.Marshal([]map[string]any{{
			"Id":   legacyWeComNetworkID(executor.accountID),
			"Name": legacyWeComNetworkName(executor.accountID), "Driver": "bridge",
			"Internal": false, "Attachable": false, "Ingress": false,
			"Labels": map[string]string{
				"dev.wechatcopilot.driver": "wecom-network", "dev.wechatcopilot.account": executor.accountID,
			},
		}})
	}
	return nil, errors.New("unexpected legacy WeCom CLI executor command")
}

func (executor *legacyWeComCLIExecutor) RunInput(
	ctx context.Context, _ []byte, _ int64, name string, args ...string,
) ([]byte, error) {
	return executor.Run(ctx, name, args...)
}

func TestAccountsApproveLegacyWeComProfileRequiresConfirmationBeforeStateAccess(t *testing.T) {
	root := t.TempDir()
	missingHome := filepath.Join(root, "must-not-be-created")
	setLegacyWeComCLIEnvironment(t, root)

	_, err := runLegacyWeComCLI(t, missingHome, nil,
		"accounts", "approve-legacy-wecom-profile", "--account", "work",
	)
	assertCLIError(t, err, api.CodeInvalidArgument, "--confirm is required for legacy WeCom profile approval")
	if _, statErr := os.Lstat(missingHome); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unconfirmed approval touched state home: %v", statErr)
	}
}

func TestAccountsApproveLegacyWeComProfileSucceedsByAlias(t *testing.T) {
	fixture := newLegacyWeComCLIFixture(t, driver.PlatformWeCom)
	stdout, err := runLegacyWeComCLI(t, fixture.paths.Home, fixture.executor,
		"accounts", "approve-legacy-wecom-profile", "--account", fixture.account.Alias, "--confirm",
	)
	if err != nil {
		t.Fatalf("approve legacy WeCom profile: %v stdout=%s", err, stdout)
	}
	var envelope struct {
		OK   bool                             `json:"ok"`
		Data legacyWeComProfileApprovalResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode approval output: %v: %s", err, stdout)
	}
	if !envelope.OK || !envelope.Data.Approved ||
		envelope.Data.AccountID != fixture.account.ID ||
		envelope.Data.AccountAlias != fixture.account.Alias ||
		envelope.Data.Platform != driver.PlatformWeCom {
		t.Fatalf("unexpected approval output: %#v", envelope)
	}
	if strings.Contains(stdout, fixture.dataDir) || strings.Contains(stdout, fixture.paths.Home) || strings.Contains(stdout, "preserved legacy login state") {
		t.Fatalf("approval output exposed a local path or profile content: %s", stdout)
	}
	approvalPath := filepath.Join(fixture.paths.Accounts, fixture.account.ID, "wecom-legacy-profile-approval.json")
	info, err := os.Lstat(approvalPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("approval file: info=%v err=%v", info, err)
	}
	for _, forbidden := range []string{
		filepath.Join(fixture.paths.Accounts, fixture.account.ID, "wecom-profile.json"),
		filepath.Join(fixture.dataDir, ".wechatcopilot-profile.json"),
	} {
		if _, err := os.Lstat(forbidden); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("CLI created a profile marker %q: %v", filepath.Base(forbidden), err)
		}
	}
}

func TestAccountsApproveLegacyWeComProfileRequiresDaemonStateLock(t *testing.T) {
	fixture := newLegacyWeComCLIFixture(t, driver.PlatformWeCom)
	lock, err := daemon.AcquireStateLock(fixture.paths.Home)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	_, err = runLegacyWeComCLI(t, fixture.paths.Home, fixture.executor,
		"accounts", "approve-legacy-wecom-profile", "--account", fixture.account.ID, "--confirm",
	)
	assertCLIError(t, err, api.CodeConflict, "stop the daemon before approving a legacy WeCom profile")
	assertNoLegacyWeComCLIApproval(t, fixture)
}

func TestAccountsApproveLegacyWeComProfileRejectsRunningExactContainer(t *testing.T) {
	fixture := newLegacyWeComCLIFixture(t, driver.PlatformWeCom)
	fixture.executor.running = true
	_, err := runLegacyWeComCLI(t, fixture.paths.Home, fixture.executor,
		"accounts", "approve-legacy-wecom-profile", "--account", fixture.account.ID, "--confirm",
	)
	assertCLIError(t, err, api.CodeConflict, "legacy WeCom profile cannot be approved")
	assertNoLegacyWeComCLIApproval(t, fixture)
	if len(fixture.executor.calls) != 1 || len(fixture.executor.calls[0].args) < 2 ||
		fixture.executor.calls[0].args[0] != "container" || fixture.executor.calls[0].args[1] != "inspect" {
		t.Fatalf("running approval performed commands after the stopped-container gate: %#v", fixture.executor.calls)
	}
}

func TestAccountsApproveLegacyWeComProfileRejectsUnknownOrNonWeComAccount(t *testing.T) {
	t.Run("unknown account", func(t *testing.T) {
		fixture := newLegacyWeComCLIFixture(t, driver.PlatformWeCom)
		_, err := runLegacyWeComCLI(t, fixture.paths.Home, fixture.executor,
			"accounts", "approve-legacy-wecom-profile", "--account", "missing", "--confirm",
		)
		assertCLIError(t, err, api.CodeNotFound, "account not found")
		assertNoLegacyWeComCLIApproval(t, fixture)
	})

	t.Run("personal WeChat account", func(t *testing.T) {
		fixture := newLegacyWeComCLIFixture(t, driver.PlatformWeChat)
		_, err := runLegacyWeComCLI(t, fixture.paths.Home, fixture.executor,
			"accounts", "approve-legacy-wecom-profile", "--account", fixture.account.ID, "--confirm",
		)
		assertCLIError(t, err, api.CodeConflict, "account is not a WeCom account")
		assertNoLegacyWeComCLIApproval(t, fixture)
	})
}

func TestAccountsApproveLegacyWeComProfileRejectsMissingOrSymlinkedData(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		fixture := newLegacyWeComCLIFixture(t, driver.PlatformWeCom)
		if err := os.RemoveAll(fixture.dataDir); err != nil {
			t.Fatal(err)
		}
		_, err := runLegacyWeComCLI(t, fixture.paths.Home, fixture.executor,
			"accounts", "approve-legacy-wecom-profile", "--account", fixture.account.ID, "--confirm",
		)
		assertCLIError(t, err, api.CodeConflict, "legacy WeCom profile cannot be approved")
		assertNoLegacyWeComCLIApproval(t, fixture)
	})

	t.Run("symlink", func(t *testing.T) {
		fixture := newLegacyWeComCLIFixture(t, driver.PlatformWeCom)
		realData := fixture.dataDir + ".real"
		if err := os.Rename(fixture.dataDir, realData); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realData, fixture.dataDir); err != nil {
			t.Fatal(err)
		}
		_, err := runLegacyWeComCLI(t, fixture.paths.Home, fixture.executor,
			"accounts", "approve-legacy-wecom-profile", "--account", fixture.account.ID, "--confirm",
		)
		assertCLIError(t, err, api.CodeConflict, "legacy WeCom profile cannot be approved")
		assertNoLegacyWeComCLIApproval(t, fixture)
	})
}

func newLegacyWeComCLIFixture(t *testing.T, platform driver.Platform) legacyWeComCLIFixture {
	t.Helper()
	root := t.TempDir()
	setLegacyWeComCLIEnvironment(t, root)
	paths, err := config.ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	paths, err = paths.WithHome(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	registry, err := account.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	item, err := registry.Add("work", platform)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(paths.Accounts, item.ID, "wecom", "android-data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(dataDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "legacy-state"), []byte("preserved legacy login state"), 0o600); err != nil {
		t.Fatal(err)
	}
	image := "registry.example/redroid@sha256:" + strings.Repeat("a", 64)
	return legacyWeComCLIFixture{
		paths: paths, account: item, dataDir: dataDir,
		executor: &legacyWeComCLIExecutor{accountID: item.ID, dataDir: dataDir, image: image},
	}
}

func setLegacyWeComCLIEnvironment(t *testing.T, root string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv(config.EnvHome, "")
	t.Setenv(config.EnvStateMountSource, "")
	t.Setenv(config.EnvStateMountFSType, "")
	t.Setenv(config.EnvStateMountUUID, "")
	t.Setenv(config.EnvStrictSwap, "")
	t.Setenv(config.EnvWeComRedroidImage, "registry.example/redroid@sha256:"+strings.Repeat("a", 64))
}

func runLegacyWeComCLI(
	t *testing.T, home string, executor wecom.Executor, args ...string,
) (string, error) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command := newRootWithDependencies(
		"test", bytes.NewReader(nil), &stdout, &stderr, func() error { return nil }, executor,
	)
	command.SetArgs(append([]string{"--home", home, "--json"}, args...))
	err := command.ExecuteContext(context.Background())
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
	return stdout.String(), err
}

func legacyWeComContainerName(accountID string) string {
	sum := sha256.Sum256([]byte(accountID))
	return "wechatcopilot-wecom-" + hex.EncodeToString(sum[:8])
}

func legacyWeComContainerID(accountID string) string {
	sum := sha256.Sum256([]byte("wecom-container:" + accountID))
	return hex.EncodeToString(sum[:])
}

func legacyWeComNetworkName(accountID string) string {
	sum := sha256.Sum256([]byte("wecom-network:" + accountID))
	return "wechatcopilot-wecom-net-" + hex.EncodeToString(sum[:8])
}

func legacyWeComNetworkID(accountID string) string {
	sum := sha256.Sum256([]byte("wecom-network-id:" + accountID))
	return hex.EncodeToString(sum[:])
}

func legacyWeComHostname(accountID string) string {
	sum := sha256.Sum256([]byte("wecom-device:" + accountID))
	return "wecom-" + hex.EncodeToString(sum[:6])
}

func assertNoLegacyWeComCLIApproval(t *testing.T, fixture legacyWeComCLIFixture) {
	t.Helper()
	path := filepath.Join(fixture.paths.Accounts, fixture.account.ID, "wecom-legacy-profile-approval.json")
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected legacy WeCom profile approval: %v", err)
	}
}
