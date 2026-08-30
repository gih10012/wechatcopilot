package wecom

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	core "github.com/gih10012/wechatcopilot/internal/driver"
)

const (
	labelDriver  = "dev.wechatcopilot.driver"
	labelAccount = "dev.wechatcopilot.account"
)

type Runtime struct {
	config    Config
	executor  Executor
	artifacts ArtifactManager

	mu            sync.Mutex
	account       core.AccountRuntime
	containerName string
	containerID   string
	networkName   string
	dataDir       string
	android       AndroidContainer
	companion     *CompanionClient
	clientVersion string
	lockFile      *os.File
	running       bool
}

func NewRuntime(config Config, executor Executor) (*Runtime, error) {
	config.normalize()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if executor == nil {
		executor = OSExecutor{}
	}
	return &Runtime{
		config:    config,
		executor:  executor,
		artifacts: ArtifactManager{Client: &http.Client{Timeout: config.DownloadTimeout}},
	}, nil
}

func (r *Runtime) Start(ctx context.Context, account core.AccountRuntime) (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return errors.New("WeCom runtime is already running")
	}
	stateDir, err := validateAccountStateDir(account.StateDir, account.AccountID)
	if err != nil {
		return err
	}
	account.StateDir = stateDir
	dataDir, err := accountDataDir(stateDir, account.AccountID)
	if err != nil {
		return err
	}
	if strings.Contains(dataDir, ",") {
		return errors.New("account data path cannot contain a comma")
	}
	accountDir := filepath.Dir(dataDir)
	if err := ensureManagedDirectory(accountDir); err != nil {
		return fmt.Errorf("prepare WeCom account directory: %w", err)
	}
	lock, err := acquireAccountLock(filepath.Join(accountDir, ".runtime.lock"))
	if err != nil {
		return err
	}
	r.lockFile = lock
	r.account = account
	r.dataDir = dataDir
	r.containerName = containerName(account.AccountID)
	r.networkName = networkName(account.AccountID)
	if err := r.preparePersistentProfile(ctx, account); err != nil {
		_ = releaseAccountLock(r.lockFile)
		r.clearRuntimeState()
		return err
	}
	cleanupContainerID := ""
	defer func() {
		if err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r.cleanupFailedStart(cleanupCtx, cleanupContainerID)
		}
	}()

	if err := r.artifacts.EnsureOfficialAPK(ctx, r.config.OfficialAPKURL, r.config.OfficialAPKPath, r.config.OfficialAPKSHA256); err != nil {
		return err
	}
	officialSnapshot, officialDigest, err := snapshotAPK(r.config.OfficialAPKPath, accountDir, r.config.OfficialAPKSHA256)
	if err != nil {
		return fmt.Errorf("snapshot official APK: %w", err)
	}
	defer func() {
		if removeErr := os.Remove(officialSnapshot); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove private official APK snapshot: %w", removeErr))
		}
	}()
	companionSnapshot, companionDigest, err := snapshotAPK(r.config.CompanionAPKPath, accountDir, "")
	if err != nil {
		return fmt.Errorf("snapshot companion APK: %w", err)
	}
	defer func() {
		if removeErr := os.Remove(companionSnapshot); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove private companion APK snapshot: %w", removeErr))
		}
	}()

	if err := r.ensureNetwork(ctx, account.AccountID); err != nil {
		return err
	}
	if err := r.verifyProfileBeforeContainerOperation(account, "inspect"); err != nil {
		return err
	}
	exists, isRunning, err := r.inspectContainer(ctx, account.AccountID, dataDir)
	if err != nil {
		return err
	}
	if !exists {
		if err := r.verifyProfileBeforeContainerOperation(account, "create"); err != nil {
			return err
		}
		if err := r.createContainer(ctx, account.AccountID, dataDir); err != nil {
			return err
		}
	}
	if !isRunning {
		if err := r.verifyProfileBeforeContainerOperation(account, "start"); err != nil {
			return err
		}
		containerID, startErr := r.startExactStoppedContainer(ctx, account.AccountID, dataDir)
		cleanupContainerID = containerID
		if startErr != nil {
			return startErr
		}
	} else {
		if err := r.verifyProfileBeforeContainerOperation(account, "reuse"); err != nil {
			return err
		}
		containerID, err := r.inspectExactRunningContainer(ctx, account.AccountID, dataDir)
		if err != nil {
			return fmt.Errorf("pin running Redroid container before reuse: %w", err)
		}
		cleanupContainerID = containerID
	}
	// A newly started Redroid container can reject docker exec briefly before
	// toybox is available. The readiness probe is immutable-ID bound and does
	// not touch /data; the full live inode/sentinel proof still precedes every
	// Android command that can observe or mutate the profile.
	if err := r.waitForContainerExecReady(ctx, account, cleanupContainerID); err != nil {
		return err
	}
	verifiedContainerID, err := r.resolveRunningContainerProfile(ctx, account)
	if err != nil {
		return err
	}
	if verifiedContainerID != cleanupContainerID {
		return errors.New("Redroid container changed before live profile proof completed")
	}
	r.containerID = verifiedContainerID
	r.android = AndroidContainer{
		DockerBinary: r.config.DockerBinary,
		Container:    r.containerName,
		Executor:     r.executor,
		Resolve: func(verifyCtx context.Context) (string, error) {
			return r.resolvePinnedRunningContainerProfile(verifyCtx, account, verifiedContainerID)
		},
	}
	if err := r.android.WaitForBoot(ctx, r.config.StartupTimeout); err != nil {
		return err
	}
	if err := r.android.DisableNetworkADB(ctx); err != nil {
		return err
	}
	if err := r.android.ProbeNetcat(ctx); err != nil {
		return err
	}
	if err := r.android.Install(ctx, officialSnapshot, containerAPKPath("wecom", officialDigest), officialDigest); err != nil {
		return err
	}
	r.clientVersion, err = r.android.PackageVersion(ctx, r.config.WeComPackage)
	if err != nil {
		return err
	}
	if err := r.android.Install(ctx, companionSnapshot, containerAPKPath("companion", companionDigest), companionDigest); err != nil {
		return err
	}
	if _, err := r.android.PackageVersion(ctx, r.config.CompanionPackage); err != nil {
		return err
	}
	if err := r.android.ConfigureCompanion(ctx, r.config.CompanionPackage); err != nil {
		return err
	}
	token, err := r.android.WaitForCompanionToken(ctx, r.config.CompanionPackage, r.config.StartupTimeout)
	if err != nil {
		return err
	}
	legacyToken := filepath.Join(accountDir, ".companion-token")
	if err := os.Remove(legacyToken); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove obsolete host companion token: %w", err)
	}
	r.companion, err = newContainerCompanionClient(r.android, DefaultCompanionPort, token, r.config.HTTPTimeout)
	if err != nil {
		return err
	}
	if err := r.waitForCompanion(ctx); err != nil {
		return err
	}
	if err := r.android.LaunchWeCom(ctx, r.config.WeComPackage); err != nil {
		return err
	}
	r.running = true
	return nil
}

func (r *Runtime) waitForContainerExecReady(
	ctx context.Context,
	account core.AccountRuntime,
	expectedContainerID string,
) error {
	if !validImmutableContainerID(expectedContainerID) {
		return errors.New("cannot wait for an invalid Redroid container identity")
	}
	timeout := min(r.config.StartupTimeout, 10*time.Second)
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := r.verifyProfileBeforeContainerOperation(account, "exec readiness probe"); err != nil {
			return err
		}
		containerID, err := r.inspectExactRunningContainerByID(
			ctx, expectedContainerID, account.AccountID, r.dataDir,
		)
		if err != nil {
			return fmt.Errorf("verify Redroid container before exec readiness probe: %w", err)
		}
		if containerID != expectedContainerID {
			return errors.New("Redroid container changed before exec readiness probe")
		}
		_, lastErr = r.executor.RunInput(
			ctx, nil, 16,
			r.config.DockerBinary, "container", "exec", "--user", "0:0", expectedContainerID,
			"/system/bin/toybox", "true",
		)
		if lastErr == nil {
			verifiedID, err := r.inspectExactRunningContainerByID(
				ctx, expectedContainerID, account.AccountID, r.dataDir,
			)
			if err != nil {
				return fmt.Errorf("verify Redroid container after exec readiness probe: %w", err)
			}
			if verifiedID != expectedContainerID {
				return errors.New("Redroid container changed during exec readiness probe")
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("wait for Redroid container exec readiness: %w", lastErr)
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *Runtime) Stop(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.containerName != "" || r.containerID != "" || r.account.AccountID != "" || r.dataDir != "" {
		if r.containerName == "" || !validImmutableContainerID(r.containerID) ||
			r.account.AccountID == "" || r.dataDir == "" {
			return errors.New("WeCom runtime container identity is incomplete")
		}
		seconds := int(r.config.StopGrace.Round(time.Second) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		if err := r.stopPinnedRuntimeContainer(
			ctx, r.containerID, r.account.AccountID, r.dataDir, seconds,
		); err != nil {
			return err
		}
	}
	if err := releaseAccountLock(r.lockFile); err != nil {
		return err
	}
	r.clearRuntimeState()
	return nil
}

func (r *Runtime) Companion() (*CompanionClient, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running || r.companion == nil {
		return nil, errors.New("WeCom runtime is not running")
	}
	return r.companion, nil
}

func (r *Runtime) Android() (AndroidContainer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return AndroidContainer{}, errors.New("WeCom runtime is not running")
	}
	return r.android, nil
}

func (r *Runtime) Account() (core.AccountRuntime, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.account, r.running
}

func (r *Runtime) ClientVersion() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clientVersion
}

type runtimeContainerInspection struct {
	ID           string `json:"Id"`
	Name         string `json:"Name"`
	RestartCount int    `json:"RestartCount"`
	Config       struct {
		Image    string            `json:"Image"`
		Hostname string            `json:"Hostname"`
		Labels   map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Paused     bool   `json:"Paused"`
		Restarting bool   `json:"Restarting"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
	} `json:"State"`
	HostConfig struct {
		Privileged      bool                         `json:"Privileged"`
		NetworkMode     string                       `json:"NetworkMode"`
		PublishAllPorts bool                         `json:"PublishAllPorts"`
		PortBindings    map[string][]json.RawMessage `json:"PortBindings"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Ports    map[string][]json.RawMessage `json:"Ports"`
		Networks map[string]json.RawMessage   `json:"Networks"`
	} `json:"NetworkSettings"`
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

type containerExecutionEpoch struct {
	ID           string
	Status       string
	StartedAt    string
	FinishedAt   string
	RestartCount int
	ExitCode     int
}

func (epoch containerExecutionEpoch) equal(other containerExecutionEpoch) bool {
	return epoch == other
}

func (r *Runtime) inspectExactRunningContainer(
	ctx context.Context,
	accountID, dataDir string,
) (string, error) {
	inspection, err := r.inspectExactContainerTarget(ctx, r.containerName, accountID, dataDir)
	if err != nil {
		return "", err
	}
	if !inspection.State.Running {
		return "", errors.New("the exact Redroid account container is not running")
	}
	return inspection.ID, nil
}

func (r *Runtime) inspectExactRunningContainerByID(
	ctx context.Context,
	containerID, accountID, dataDir string,
) (string, error) {
	if !validImmutableContainerID(containerID) {
		return "", errors.New("the pinned Redroid container identity is invalid")
	}
	// Once startup has pinned a full immutable ID, the Docker name is no
	// longer part of the identity. An external rename or reuse of the old name
	// must not redirect or disable operations on this exact container.
	inspection, err := r.inspectPinnedContainerTarget(ctx, containerID, accountID, dataDir)
	if err != nil {
		return "", err
	}
	if inspection.ID != containerID {
		return "", errors.New("the pinned Redroid container identity changed")
	}
	if !inspection.State.Running {
		return "", errors.New("the pinned Redroid account container is not running")
	}
	return inspection.ID, nil
}

func (r *Runtime) stopPinnedRuntimeContainer(
	ctx context.Context,
	containerID, accountID, dataDir string,
	seconds int,
) error {
	inspection, exists, err := r.inspectPinnedRuntimeContainer(
		ctx, containerID, accountID, dataDir,
	)
	if err != nil {
		return fmt.Errorf("verify pinned Redroid container before stop: %w", err)
	}
	if !exists {
		return nil
	}
	if inspection.State.Running {
		if _, err := r.executor.Run(
			ctx, r.config.DockerBinary,
			"container", "stop", "--time", strconv.Itoa(seconds), containerID,
		); err != nil {
			return fmt.Errorf("stop pinned Redroid container: %w", err)
		}
		inspection, exists, err = r.inspectPinnedRuntimeContainer(
			ctx, containerID, accountID, dataDir,
		)
		if err != nil {
			return fmt.Errorf("verify pinned Redroid container after stop: %w", err)
		}
		if !exists {
			return errors.New("pinned Redroid container disappeared during stop")
		}
	}
	if _, err := stoppedContainerExecutionEpoch(inspection); err != nil {
		return fmt.Errorf("pinned Redroid container did not reach an exact stopped state: %w", err)
	}
	return nil
}

func (r *Runtime) startExactStoppedContainer(
	ctx context.Context,
	accountID, dataDir string,
) (string, error) {
	containerID, err := r.inspectExactStoppedContainer(ctx, accountID, dataDir)
	if err != nil {
		return "", fmt.Errorf("pin stopped Redroid container before start: %w", err)
	}
	if _, err := r.executor.Run(ctx, r.config.DockerBinary, "container", "start", containerID); err != nil {
		return containerID, fmt.Errorf("start Redroid container: %w", err)
	}
	startedContainerID, err := r.inspectExactRunningContainer(ctx, accountID, dataDir)
	if err != nil {
		return containerID, fmt.Errorf("verify Redroid container after start: %w", err)
	}
	if startedContainerID != containerID {
		return containerID, errors.New("Redroid container changed during start")
	}
	return containerID, nil
}

func (r *Runtime) stopExactRunningContainer(
	ctx context.Context,
	accountID, dataDir string,
	seconds int,
) (string, error) {
	containerID, err := r.inspectExactRunningContainer(ctx, accountID, dataDir)
	if err != nil {
		return "", fmt.Errorf("pin running Redroid container before stop: %w", err)
	}
	if _, err := r.executor.Run(
		ctx, r.config.DockerBinary,
		"container", "stop", "--time", strconv.Itoa(seconds), containerID,
	); err != nil {
		return containerID, fmt.Errorf("stop Redroid container: %w", err)
	}
	stoppedContainerID, err := r.inspectExactStoppedContainer(ctx, accountID, dataDir)
	if err != nil {
		return containerID, fmt.Errorf("verify Redroid container after stop: %w", err)
	}
	if stoppedContainerID != containerID {
		return containerID, errors.New("Redroid container changed during stop")
	}
	return containerID, nil
}

func (r *Runtime) inspectExactStoppedContainer(
	ctx context.Context,
	accountID, dataDir string,
) (string, error) {
	epoch, err := r.inspectExactStoppedContainerEpoch(ctx, accountID, dataDir)
	if err != nil {
		return "", err
	}
	return epoch.ID, nil
}

func (r *Runtime) inspectExactStoppedContainerEpoch(
	ctx context.Context,
	accountID, dataDir string,
) (containerExecutionEpoch, error) {
	inspection, err := r.inspectExactContainerTarget(ctx, r.containerName, accountID, dataDir)
	if err != nil {
		return containerExecutionEpoch{}, err
	}
	return stoppedContainerExecutionEpoch(inspection)
}

func stoppedContainerExecutionEpoch(inspection runtimeContainerInspection) (containerExecutionEpoch, error) {
	if !validImmutableContainerID(inspection.ID) {
		return containerExecutionEpoch{}, errors.New("the exact Redroid account container identity is missing or invalid")
	}
	if inspection.State.Running {
		return containerExecutionEpoch{}, errors.New("the exact Redroid account container is still running")
	}
	if inspection.State.Paused || inspection.State.Restarting {
		return containerExecutionEpoch{}, errors.New("the exact Redroid account container is not safely stopped")
	}
	if inspection.State.Status != "created" && inspection.State.Status != "exited" {
		return containerExecutionEpoch{}, errors.New("the exact Redroid account container has an invalid stopped state")
	}
	if _, err := time.Parse(time.RFC3339Nano, inspection.State.StartedAt); err != nil {
		return containerExecutionEpoch{}, errors.New("the exact Redroid account container has an invalid start epoch")
	}
	if _, err := time.Parse(time.RFC3339Nano, inspection.State.FinishedAt); err != nil {
		return containerExecutionEpoch{}, errors.New("the exact Redroid account container has an invalid finish epoch")
	}
	if inspection.RestartCount < 0 {
		return containerExecutionEpoch{}, errors.New("the exact Redroid account container has an invalid restart count")
	}
	return containerExecutionEpoch{
		ID:           inspection.ID,
		Status:       inspection.State.Status,
		StartedAt:    inspection.State.StartedAt,
		FinishedAt:   inspection.State.FinishedAt,
		RestartCount: inspection.RestartCount,
		ExitCode:     inspection.State.ExitCode,
	}, nil
}

func (r *Runtime) inspectExactContainerTarget(
	ctx context.Context,
	target, accountID, dataDir string,
) (runtimeContainerInspection, error) {
	inspection, err := r.inspectPinnedContainerTarget(ctx, target, accountID, dataDir)
	if err != nil {
		return runtimeContainerInspection{}, err
	}
	if inspection.Name != "/"+r.containerName {
		return runtimeContainerInspection{}, fmt.Errorf("%w: existing Redroid container name changed", ErrClientIncompatible)
	}
	return inspection, nil
}

func (r *Runtime) inspectPinnedContainerTarget(
	ctx context.Context,
	target, accountID, dataDir string,
) (runtimeContainerInspection, error) {
	out, err := r.executor.Run(ctx, r.config.DockerBinary, "container", "inspect", target)
	if err != nil {
		return runtimeContainerInspection{}, fmt.Errorf("inspect exact Redroid container: %w", err)
	}
	var inspections []runtimeContainerInspection
	if err := json.Unmarshal(out, &inspections); err != nil || len(inspections) != 1 {
		return runtimeContainerInspection{}, errors.New("cannot decode a unique exact Redroid container inspection")
	}
	inspection := inspections[0]
	if validImmutableContainerID(target) && inspection.ID != target {
		return runtimeContainerInspection{}, errors.New("pinned Redroid container identity changed during inspection")
	}
	if err := verifyRuntimeContainerFingerprint(
		inspection, r.networkName, accountID, r.config.RedroidImage, dataDir,
	); err != nil {
		return runtimeContainerInspection{}, err
	}
	if !validImmutableContainerID(inspection.ID) {
		return runtimeContainerInspection{}, errors.New("exact Redroid container identity is missing or invalid")
	}
	return inspection, nil
}

func (r *Runtime) inspectPinnedRuntimeContainer(
	ctx context.Context,
	containerID, accountID, dataDir string,
) (runtimeContainerInspection, bool, error) {
	if !validImmutableContainerID(containerID) {
		return runtimeContainerInspection{}, false, errors.New("pinned Redroid container identity is invalid")
	}
	inspection, err := r.inspectPinnedContainerTarget(ctx, containerID, accountID, dataDir)
	if err == nil {
		return inspection, true, nil
	}
	listed, listErr := r.executor.Run(
		ctx, r.config.DockerBinary,
		"container", "ls", "--all", "--no-trunc", "--filter", "id="+containerID, "--format", "{{.ID}}",
	)
	if listErr != nil {
		return runtimeContainerInspection{}, false, err
	}
	ids := strings.Fields(string(listed))
	if len(ids) == 0 {
		return runtimeContainerInspection{}, false, nil
	}
	if len(ids) != 1 || ids[0] != containerID {
		return runtimeContainerInspection{}, true, errors.New("cannot prove the exact pinned Redroid container identity")
	}
	return runtimeContainerInspection{}, true, err
}

func validImmutableContainerID(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func (r *Runtime) inspectContainer(ctx context.Context, accountID, dataDir string) (exists, running bool, err error) {
	inspection, exists, err := r.inspectNamedRuntimeContainer(ctx, accountID, dataDir)
	if err != nil || !exists {
		return exists, false, err
	}
	return true, inspection.State.Running, nil
}

func (r *Runtime) inspectNamedRuntimeContainer(
	ctx context.Context,
	accountID, dataDir string,
) (runtimeContainerInspection, bool, error) {
	out, inspectErr := r.executor.Run(ctx, r.config.DockerBinary, "container", "inspect", r.containerName)
	if inspectErr != nil {
		listed, listErr := r.executor.Run(
			ctx,
			r.config.DockerBinary,
			"container", "ls", "--all", "--filter", "name=^/"+r.containerName+"$", "--format", "{{.Names}}",
		)
		if listErr != nil {
			return runtimeContainerInspection{}, false, fmt.Errorf("inspect Redroid container: %w", inspectErr)
		}
		if strings.TrimSpace(string(listed)) == "" {
			return runtimeContainerInspection{}, false, nil
		}
		return runtimeContainerInspection{}, true, fmt.Errorf("inspect existing Redroid container: %w", inspectErr)
	}
	var inspections []runtimeContainerInspection
	if err := json.Unmarshal(out, &inspections); err != nil || len(inspections) != 1 {
		return runtimeContainerInspection{}, true, errors.New("cannot decode a unique Redroid container inspection")
	}
	inspection := inspections[0]
	if err := verifyRuntimeContainer(inspection, r.containerName, r.networkName, accountID, r.config.RedroidImage, dataDir); err != nil {
		return runtimeContainerInspection{}, true, err
	}
	return inspection, true, nil
}

func verifyRuntimeContainer(inspection runtimeContainerInspection, expectedName, expectedNetwork, accountID, image, dataDir string) error {
	if inspection.Name != "/"+expectedName {
		return fmt.Errorf("%w: existing Redroid container does not match the exact account container name", ErrClientIncompatible)
	}
	return verifyRuntimeContainerFingerprint(inspection, expectedNetwork, accountID, image, dataDir)
}

func verifyRuntimeContainerFingerprint(inspection runtimeContainerInspection, expectedNetwork, accountID, image, dataDir string) error {
	if inspection.Config.Labels[labelDriver] != "wecom" ||
		inspection.Config.Labels[labelAccount] != accountID ||
		inspection.Config.Image != image ||
		inspection.Config.Hostname != containerHostname(accountID) {
		return fmt.Errorf("%w: existing Redroid container does not match the exact account, image, hostname, and ownership fingerprint", ErrClientIncompatible)
	}
	if !inspection.HostConfig.Privileged || inspection.HostConfig.NetworkMode != expectedNetwork {
		return fmt.Errorf("%w: existing Redroid container does not match the required privileged runtime and isolated account network", ErrClientIncompatible)
	}
	if inspection.HostConfig.PublishAllPorts || hasPublishedPorts(inspection.HostConfig.PortBindings) || hasPublishedPorts(inspection.NetworkSettings.Ports) {
		return fmt.Errorf("%w: refusing to reuse a Redroid container with host-published ports; remove it and recreate the account runtime safely", ErrClientIncompatible)
	}
	if len(inspection.NetworkSettings.Networks) != 1 {
		return fmt.Errorf("%w: Redroid container must be attached only to its isolated account network", ErrClientIncompatible)
	}
	if _, ok := inspection.NetworkSettings.Networks[expectedNetwork]; !ok {
		return fmt.Errorf("%w: Redroid container is not attached to its isolated account network", ErrClientIncompatible)
	}
	if len(inspection.Mounts) != 1 {
		return fmt.Errorf("%w: Redroid container must have exactly one account data mount", ErrClientIncompatible)
	}
	mount := inspection.Mounts[0]
	if mount.Type != "bind" || !mount.RW || mount.Destination != "/data" || canonicalPath(mount.Source) != canonicalPath(dataDir) {
		return fmt.Errorf("%w: Redroid /data bind mount does not exactly match the account", ErrClientIncompatible)
	}
	return nil
}

func hasPublishedPorts(ports map[string][]json.RawMessage) bool {
	for _, bindings := range ports {
		if len(bindings) != 0 {
			return true
		}
	}
	return false
}

type runtimeNetworkInspection struct {
	ID         string            `json:"Id"`
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Internal   bool              `json:"Internal"`
	Attachable bool              `json:"Attachable"`
	Ingress    bool              `json:"Ingress"`
	Labels     map[string]string `json:"Labels"`
}

func (r *Runtime) ensureNetwork(ctx context.Context, accountID string) error {
	exists, err := r.inspectNetwork(ctx, accountID)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	args := []string{
		"network", "create",
		"--driver", "bridge",
		"--label", labelDriver + "=wecom-network",
		"--label", labelAccount + "=" + accountID,
		r.networkName,
	}
	if _, err := r.executor.Run(ctx, r.config.DockerBinary, args...); err != nil {
		return fmt.Errorf("create isolated WeCom account network: %w", err)
	}
	exists, err = r.inspectNetwork(ctx, accountID)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("isolated WeCom account network disappeared after creation")
	}
	return nil
}

func (r *Runtime) inspectNetwork(ctx context.Context, accountID string) (bool, error) {
	return inspectAccountNetwork(ctx, r.executor, r.config.DockerBinary, r.networkName, accountID)
}

func inspectAccountNetwork(ctx context.Context, executor Executor, dockerBinary, expectedName, accountID string) (bool, error) {
	_, exists, err := inspectAccountNetworkIdentity(ctx, executor, dockerBinary, expectedName, accountID)
	return exists, err
}

func inspectAccountNetworkIdentity(
	ctx context.Context,
	executor Executor,
	dockerBinary, expectedName, accountID string,
) (string, bool, error) {
	out, inspectErr := executor.Run(ctx, dockerBinary, "network", "inspect", expectedName)
	if inspectErr != nil {
		listed, listErr := executor.Run(ctx, dockerBinary, "network", "ls", "--filter", "name=^"+expectedName+"$", "--format", "{{.Name}}")
		if listErr != nil {
			return "", false, fmt.Errorf("inspect isolated WeCom network: %w", inspectErr)
		}
		if strings.TrimSpace(string(listed)) == "" {
			return "", false, nil
		}
		return "", true, fmt.Errorf("inspect existing isolated WeCom network: %w", inspectErr)
	}
	var inspections []runtimeNetworkInspection
	if err := json.Unmarshal(out, &inspections); err != nil || len(inspections) != 1 {
		return "", true, errors.New("cannot decode a unique isolated WeCom network inspection")
	}
	inspection := inspections[0]
	if inspection.Name != expectedName || inspection.Driver != "bridge" || inspection.Internal || inspection.Attachable || inspection.Ingress ||
		inspection.Labels[labelDriver] != "wecom-network" || inspection.Labels[labelAccount] != accountID {
		return "", true, fmt.Errorf("%w: existing Docker network does not match the exact isolated WeCom account fingerprint", ErrClientIncompatible)
	}
	if !validImmutableContainerID(inspection.ID) {
		return "", true, errors.New("isolated WeCom account network identity is missing or invalid")
	}
	return inspection.ID, true, nil
}

func removeAccountNetwork(
	ctx context.Context,
	executor Executor,
	dockerBinary, expectedName, accountID, expectedID string,
) error {
	currentID, exists, err := inspectAccountNetworkIdentity(ctx, executor, dockerBinary, expectedName, accountID)
	if err != nil {
		return err
	}
	if !exists {
		if expectedID == "" {
			return nil
		}
		return errors.New("isolated WeCom account network disappeared before exact removal")
	}
	if !validImmutableContainerID(expectedID) || currentID != expectedID {
		return errors.New("isolated WeCom account network changed before exact removal")
	}
	if _, err := executor.Run(ctx, dockerBinary, "network", "rm", expectedID); err != nil {
		return fmt.Errorf("remove isolated WeCom account network: %w", err)
	}
	remainingID, remains, err := inspectAccountNetworkIdentity(ctx, executor, dockerBinary, expectedName, accountID)
	if err != nil {
		return fmt.Errorf("verify isolated WeCom account network after removal: %w", err)
	}
	if remains {
		return fmt.Errorf("isolated WeCom account network name changed during removal (now %s)", remainingID)
	}
	return nil
}

func (r *Runtime) createContainer(ctx context.Context, accountID, dataDir string) error {
	args := []string{
		"container", "create",
		"--pull", "never",
		"--name", r.containerName,
		"--hostname", containerHostname(accountID),
		"--privileged",
		"--network", r.networkName,
		"--label", labelDriver + "=wecom",
		"--label", labelAccount + "=" + accountID,
		"--mount", "type=bind,src=" + dataDir + ",dst=/data",
		"--env", "androidboot.redroid_gpu_mode=guest",
		r.config.RedroidImage,
	}
	if _, err := r.executor.Run(ctx, r.config.DockerBinary, args...); err != nil {
		return fmt.Errorf("create pinned Redroid container: %w", err)
	}
	return nil
}

func (r *Runtime) waitForCompanion(ctx context.Context) error {
	deadline := time.Now().Add(r.config.StartupTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := r.companion.Health(probeCtx)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for accessibility companion")
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *Runtime) cleanupFailedStart(ctx context.Context, containerID string) {
	if validImmutableContainerID(containerID) {
		_, _ = r.executor.Run(ctx, r.config.DockerBinary, "container", "stop", "--time", "5", containerID)
	}
	_ = releaseAccountLock(r.lockFile)
	r.clearRuntimeState()
}

func (r *Runtime) clearRuntimeState() {
	r.lockFile = nil
	r.companion = nil
	r.android = AndroidContainer{}
	r.account = core.AccountRuntime{}
	r.containerName = ""
	r.containerID = ""
	r.networkName = ""
	r.dataDir = ""
	r.clientVersion = ""
	r.running = false
}

func acquireAccountLock(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open account runtime lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("open account runtime lock returned an invalid descriptor")
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Uid != uint32(os.Geteuid()) {
		_ = f.Close()
		return nil, errors.New("account runtime lock must be a regular file owned by the daemon user")
	}
	if err := syscall.Fchmod(fd, 0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("protect account runtime lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("account runtime is already active")
	}
	return f, nil
}

func releaseAccountLock(f *os.File) error {
	if f == nil {
		return nil
	}
	// Closing the descriptor releases its BSD flock atomically with the
	// process losing access to that lock handle.
	return f.Close()
}

func containerName(accountID string) string {
	sum := sha256.Sum256([]byte(accountID))
	return "wechatcopilot-wecom-" + hex.EncodeToString(sum[:8])
}

func networkName(accountID string) string {
	sum := sha256.Sum256([]byte("wecom-network:" + accountID))
	return "wechatcopilot-wecom-net-" + hex.EncodeToString(sum[:8])
}

func containerHostname(accountID string) string {
	sum := sha256.Sum256([]byte("wecom-device:" + accountID))
	return "wecom-" + hex.EncodeToString(sum[:6])
}

func containerAPKPath(role, digest string) string {
	return "/data/local/tmp/wechatcopilot-" + role + "-" + strings.ToLower(digest[:16]) + ".apk"
}
