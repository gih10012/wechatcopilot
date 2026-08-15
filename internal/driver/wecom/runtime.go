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
	if err := validateAccountID(account.AccountID); err != nil {
		return err
	}
	dataDir, err := accountDataDir(account.StateDir, account.AccountID)
	if err != nil {
		return err
	}
	if strings.Contains(dataDir, ",") {
		return errors.New("account data path cannot contain a comma")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create account Android data directory: %w", err)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return fmt.Errorf("protect account Android data directory: %w", err)
	}
	accountDir := filepath.Dir(dataDir)
	lock, err := acquireAccountLock(filepath.Join(accountDir, ".runtime.lock"))
	if err != nil {
		return err
	}
	r.lockFile = lock
	r.account = account
	r.dataDir = dataDir
	r.containerName = containerName(account.AccountID)
	r.networkName = networkName(account.AccountID)
	defer func() {
		if err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r.cleanupFailedStart(cleanupCtx)
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
	exists, isRunning, err := r.inspectContainer(ctx, account.AccountID, dataDir)
	if err != nil {
		return err
	}
	if !exists {
		if err := r.createContainer(ctx, account.AccountID, dataDir); err != nil {
			return err
		}
	}
	if !isRunning {
		if _, err := r.executor.Run(ctx, r.config.DockerBinary, "container", "start", r.containerName); err != nil {
			return fmt.Errorf("start Redroid container: %w", err)
		}
	}
	r.android = AndroidContainer{
		DockerBinary: r.config.DockerBinary,
		Container:    r.containerName,
		Executor:     r.executor,
		Verify: func(verifyCtx context.Context) error {
			exists, running, verifyErr := r.inspectContainer(verifyCtx, account.AccountID, dataDir)
			if verifyErr != nil {
				return verifyErr
			}
			if !exists || !running {
				return errors.New("the exact Redroid account container is not running")
			}
			return nil
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

func (r *Runtime) Stop(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.containerName != "" && r.account.AccountID != "" && r.dataDir != "" {
		exists, running, err := r.inspectContainer(ctx, r.account.AccountID, r.dataDir)
		if err != nil {
			return fmt.Errorf("verify Redroid container before stop: %w", err)
		} else if exists && running {
			seconds := int(r.config.StopGrace.Round(time.Second) / time.Second)
			if seconds < 1 {
				seconds = 1
			}
			if _, err := r.executor.Run(ctx, r.config.DockerBinary, "container", "stop", "--time", strconv.Itoa(seconds), r.containerName); err != nil {
				return fmt.Errorf("stop Redroid container: %w", err)
			}
			exists, running, err = r.inspectContainer(ctx, r.account.AccountID, r.dataDir)
			if err != nil {
				return fmt.Errorf("verify Redroid container after stop: %w", err)
			}
			if exists && running {
				return errors.New("Redroid container remained running after stop")
			}
		}
	}
	if err := releaseAccountLock(r.lockFile); err != nil {
		return err
	}
	r.lockFile = nil
	r.companion = nil
	r.android = AndroidContainer{}
	r.running = false
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
	Name   string `json:"Name"`
	Config struct {
		Image    string            `json:"Image"`
		Hostname string            `json:"Hostname"`
		Labels   map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Running bool `json:"Running"`
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

func (r *Runtime) inspectContainer(ctx context.Context, accountID, dataDir string) (exists, running bool, err error) {
	out, inspectErr := r.executor.Run(ctx, r.config.DockerBinary, "container", "inspect", r.containerName)
	if inspectErr != nil {
		listed, listErr := r.executor.Run(
			ctx,
			r.config.DockerBinary,
			"container", "ls", "--all", "--filter", "name=^/"+r.containerName+"$", "--format", "{{.Names}}",
		)
		if listErr != nil {
			return false, false, fmt.Errorf("inspect Redroid container: %w", inspectErr)
		}
		if strings.TrimSpace(string(listed)) == "" {
			return false, false, nil
		}
		return true, false, fmt.Errorf("inspect existing Redroid container: %w", inspectErr)
	}
	var inspections []runtimeContainerInspection
	if err := json.Unmarshal(out, &inspections); err != nil || len(inspections) != 1 {
		return true, false, errors.New("cannot decode a unique Redroid container inspection")
	}
	inspection := inspections[0]
	if err := verifyRuntimeContainer(inspection, r.containerName, r.networkName, accountID, r.config.RedroidImage, dataDir); err != nil {
		return true, false, err
	}
	return true, inspection.State.Running, nil
}

func verifyRuntimeContainer(inspection runtimeContainerInspection, expectedName, expectedNetwork, accountID, image, dataDir string) error {
	if inspection.Name != "/"+expectedName ||
		inspection.Config.Labels[labelDriver] != "wecom" ||
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
	out, inspectErr := executor.Run(ctx, dockerBinary, "network", "inspect", expectedName)
	if inspectErr != nil {
		listed, listErr := executor.Run(ctx, dockerBinary, "network", "ls", "--filter", "name=^"+expectedName+"$", "--format", "{{.Name}}")
		if listErr != nil {
			return false, fmt.Errorf("inspect isolated WeCom network: %w", inspectErr)
		}
		if strings.TrimSpace(string(listed)) == "" {
			return false, nil
		}
		return true, fmt.Errorf("inspect existing isolated WeCom network: %w", inspectErr)
	}
	var inspections []runtimeNetworkInspection
	if err := json.Unmarshal(out, &inspections); err != nil || len(inspections) != 1 {
		return true, errors.New("cannot decode a unique isolated WeCom network inspection")
	}
	inspection := inspections[0]
	if inspection.Name != expectedName || inspection.Driver != "bridge" || inspection.Internal || inspection.Attachable || inspection.Ingress ||
		inspection.Labels[labelDriver] != "wecom-network" || inspection.Labels[labelAccount] != accountID {
		return true, fmt.Errorf("%w: existing Docker network does not match the exact isolated WeCom account fingerprint", ErrClientIncompatible)
	}
	return true, nil
}

func removeAccountNetwork(ctx context.Context, executor Executor, dockerBinary, expectedName, accountID string) error {
	exists, err := inspectAccountNetwork(ctx, executor, dockerBinary, expectedName, accountID)
	if err != nil || !exists {
		return err
	}
	if _, err := executor.Run(ctx, dockerBinary, "network", "rm", expectedName); err != nil {
		return fmt.Errorf("remove isolated WeCom account network: %w", err)
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

func (r *Runtime) cleanupFailedStart(ctx context.Context) {
	if r.containerName != "" && r.account.AccountID != "" && r.dataDir != "" {
		exists, running, err := r.inspectContainer(ctx, r.account.AccountID, r.dataDir)
		if err == nil && exists && running {
			_, _ = r.executor.Run(ctx, r.config.DockerBinary, "container", "stop", "--time", "5", r.containerName)
		}
	}
	_ = releaseAccountLock(r.lockFile)
	r.lockFile = nil
	r.companion = nil
	r.android = AndroidContainer{}
	r.running = false
}

func acquireAccountLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open account runtime lock: %w", err)
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
