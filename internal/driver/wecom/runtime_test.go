package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	core "github.com/gih10012/wechatcopilot/internal/driver"
)

type executorCall struct {
	name string
	args []string
}

type recordingExecutor struct {
	output []byte
	err    error
	calls  []executorCall
}

type executorResult struct {
	output []byte
	err    error
}

type sequenceExecutor struct {
	results []executorResult
	calls   int
}

type functionExecutor func(context.Context, string, ...string) ([]byte, error)

func (f functionExecutor) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

func (f functionExecutor) RunInput(ctx context.Context, _ []byte, _ int64, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

func (e *sequenceExecutor) Run(_ context.Context, _ string, _ ...string) ([]byte, error) {
	if e.calls >= len(e.results) {
		return nil, errors.New("unexpected executor call")
	}
	result := e.results[e.calls]
	e.calls++
	return result.output, result.err
}

func (e *sequenceExecutor) RunInput(ctx context.Context, _ []byte, _ int64, name string, args ...string) ([]byte, error) {
	return e.Run(ctx, name, args...)
}

func (e *recordingExecutor) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	e.calls = append(e.calls, executorCall{name: name, args: append([]string(nil), args...)})
	return e.output, e.err
}

func (e *recordingExecutor) RunInput(ctx context.Context, _ []byte, _ int64, name string, args ...string) ([]byte, error) {
	return e.Run(ctx, name, args...)
}

func TestCreateContainerUsesPinnedImageWithoutPublishedPorts(t *testing.T) {
	config := validTestConfig(t)
	executor := &recordingExecutor{}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	runtime.containerName = containerName("work")
	runtime.networkName = networkName("work")
	dataDir := t.TempDir()
	if err := runtime.createContainer(context.Background(), "work", dataDir); err != nil {
		t.Fatal(err)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("expected one command, got %d", len(executor.calls))
	}
	args := executor.calls[0].args
	for _, argument := range args {
		if argument == "--publish" || strings.Contains(argument, "5555") {
			t.Fatalf("container publishes a host port: %v", args)
		}
	}
	if !containsArgumentPair(args, "--network", networkName("work")) {
		t.Fatalf("container does not use its isolated account network: %v", args)
	}
	if !containsArgumentPair(args, "--pull", "never") {
		t.Fatalf("container creation permits an implicit image pull: %v", args)
	}
	if args[len(args)-1] != config.RedroidImage {
		t.Fatalf("unexpected image: %q", args[len(args)-1])
	}
}

func TestInspectContainerRejectsForeignContainer(t *testing.T) {
	config := validTestConfig(t)
	dataDir := t.TempDir()
	executor := &recordingExecutor{output: runtimeInspection(t, "other", "work", config.RedroidImage, dataDir, true, false)}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	runtime.containerName = containerName("work")
	runtime.networkName = networkName("work")
	exists, _, err := runtime.inspectContainer(context.Background(), "work", dataDir)
	if !exists || err == nil {
		t.Fatalf("expected foreign container rejection, exists=%v err=%v", exists, err)
	}
}

func TestInspectContainerRejectsAnyPublishedPort(t *testing.T) {
	config := validTestConfig(t)
	dataDir := t.TempDir()
	executor := &recordingExecutor{output: runtimeInspection(t, "wecom", "work", config.RedroidImage, dataDir, true, true)}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	runtime.containerName = containerName("work")
	runtime.networkName = networkName("work")
	exists, _, err := runtime.inspectContainer(context.Background(), "work", dataDir)
	if !exists || err == nil || !strings.Contains(err.Error(), "host-published ports") {
		t.Fatalf("expected unsafe legacy port binding rejection, exists=%v err=%v", exists, err)
	}
}

func TestStopRetainsRuntimeAndLockWhenContainerStopFails(t *testing.T) {
	config := validTestConfig(t)
	dataDir := t.TempDir()
	stopFailure := errors.New("synthetic stop failure")
	executor := &sequenceExecutor{results: []executorResult{
		{output: runtimeInspection(t, "wecom", "work", config.RedroidImage, dataDir, true, false)},
		{err: stopFailure},
	}}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := acquireAccountLock(filepath.Join(t.TempDir(), ".runtime.lock"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.account = core.AccountRuntime{AccountID: "work"}
	runtime.dataDir = dataDir
	runtime.containerName = containerName("work")
	runtime.networkName = networkName("work")
	runtime.lockFile = lock
	runtime.running = true
	runtime.companion = &CompanionClient{}
	runtime.android = AndroidContainer{Container: runtime.containerName}
	if err := runtime.Stop(context.Background()); !errors.Is(err, stopFailure) {
		t.Fatalf("expected stop failure, got %v", err)
	}
	if !runtime.running || runtime.lockFile != lock || runtime.companion == nil || runtime.android.Container == "" {
		t.Fatal("failed stop cleared live runtime state or released its exclusion lock")
	}
	if err := releaseAccountLock(lock); err != nil {
		t.Fatal(err)
	}
	runtime.lockFile = nil
}

func TestInspectMissingContainer(t *testing.T) {
	config := validTestConfig(t)
	executor := &sequenceExecutor{results: []executorResult{
		{err: errors.New("not found")},
		{output: nil},
	}}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	runtime.containerName = containerName("work")
	runtime.networkName = networkName("work")
	exists, running, err := runtime.inspectContainer(context.Background(), "work", t.TempDir())
	if err != nil || exists || running {
		t.Fatalf("unexpected result: exists=%v running=%v err=%v", exists, running, err)
	}
}

func TestPurgeRefusesForeignContainerLabels(t *testing.T) {
	config := validTestConfig(t)
	stateDir := t.TempDir()
	dataDir, err := accountDataDir(stateDir, "work")
	if err != nil {
		t.Fatal(err)
	}
	executor := &sequenceExecutor{results: []executorResult{
		{output: networkInspection(t, "work")},
		{output: purgeInspection(t, containerName("work"), "other", "work", config.RedroidImage, dataDir, false)},
	}}
	driver, err := New(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	err = driver.Purge(context.Background(), core.AccountRuntime{AccountID: "work", StateDir: stateDir})
	if err == nil {
		t.Fatal("expected purge to reject foreign ownership labels")
	}
	if executor.calls != 2 {
		t.Fatalf("purge issued unexpected command count: %d", executor.calls)
	}
}

func TestVerifyPurgeContainerRejectsWrongDataMount(t *testing.T) {
	config := validTestConfig(t)
	expected := filepath.Join(t.TempDir(), "expected")
	other := filepath.Join(t.TempDir(), "other")
	raw := purgeInspection(t, containerName("work"), "wecom", "work", config.RedroidImage, other, false)
	if err := verifyPurgeContainer(raw, containerName("work"), "work", config.RedroidImage, expected); err == nil {
		t.Fatal("expected mismatched /data bind source to be rejected")
	}
}

func TestPurgeRemovesExactInactiveContainer(t *testing.T) {
	config := validTestConfig(t)
	stateDir := t.TempDir()
	dataDir, err := accountDataDir(stateDir, "work")
	if err != nil {
		t.Fatal(err)
	}
	executor := &sequenceExecutor{results: []executorResult{
		{output: networkInspection(t, "work")},
		{output: purgeInspection(t, containerName("work"), "wecom", "work", config.RedroidImage, dataDir, false)},
		{output: []byte("cleaned\n")},
		{output: []byte("removed\n")},
		{output: networkInspection(t, "work")},
		{output: []byte("network removed\n")},
	}}
	driver, err := New(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Purge(context.Background(), core.AccountRuntime{AccountID: "work", StateDir: stateDir}); err != nil {
		t.Fatal(err)
	}
	if executor.calls != 6 {
		t.Fatalf("expected network/container verification, cleanup, and exact removals, got %d calls", executor.calls)
	}
}

func TestPurgeClearsContainerOwnedTreeBeforeCoreRemoval(t *testing.T) {
	config := validTestConfig(t)
	stateDir := t.TempDir()
	dataDir, err := accountDataDir(stateDir, "work")
	if err != nil {
		t.Fatal(err)
	}
	lockedDir := filepath.Join(dataDir, "root-owned-simulation")
	if err := os.MkdirAll(lockedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockedDir, "state"), []byte("login state"), 0o600); err != nil {
		t.Fatal(err)
	}
	var cleanupChecked, removeChecked, networkRemoveChecked bool
	executor := functionExecutor(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch {
		case len(args) == 3 && args[0] == "network" && args[1] == "inspect":
			return networkInspection(t, "work"), nil
		case len(args) == 3 && args[0] == "network" && args[1] == "rm" && args[2] == networkName("work"):
			networkRemoveChecked = true
			return nil, nil
		case len(args) >= 3 && args[0] == "container" && args[1] == "inspect":
			return purgeInspection(t, containerName("work"), "wecom", "work", config.RedroidImage, dataDir, false), nil
		case len(args) >= 3 && args[0] == "container" && args[1] == "run":
			cleanupChecked = true
			if !containsArgumentPair(args, "--network", "none") ||
				!containsArgumentPair(args, "--entrypoint", "/system/bin/sh") ||
				!containsArgumentPair(args, "--mount", "type=bind,src="+dataDir+",dst=/account-data") {
				t.Fatalf("cleanup container is not sufficiently constrained: %v", args)
			}
			if err := os.RemoveAll(lockedDir); err != nil {
				return nil, err
			}
			return nil, nil
		case len(args) == 3 && args[0] == "container" && args[1] == "rm" && args[2] == containerName("work"):
			removeChecked = true
			return nil, nil
		default:
			return nil, errors.New("unexpected Docker command")
		}
	})
	driver, err := New(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Purge(context.Background(), core.AccountRuntime{AccountID: "work", StateDir: stateDir}); err != nil {
		t.Fatal(err)
	}
	if !cleanupChecked || !removeChecked || !networkRemoveChecked {
		t.Fatalf("cleanup=%v remove=%v network_remove=%v", cleanupChecked, removeChecked, networkRemoveChecked)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected Android data tree to be gone, got %v", err)
	}
}

func purgeInspection(t *testing.T, name, owner, accountID, image, dataDir string, running bool) []byte {
	t.Helper()
	value := []map[string]any{{
		"Name": "/" + name,
		"Config": map[string]any{
			"Image":  image,
			"Labels": map[string]string{labelDriver: owner, labelAccount: accountID},
		},
		"State": map[string]bool{"Running": running},
		"Mounts": []map[string]any{{
			"Type": "bind", "Source": dataDir, "Destination": "/data", "RW": true,
		}},
	}}
	contents, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func runtimeInspection(t *testing.T, owner, accountID, image, dataDir string, running, published bool) []byte {
	t.Helper()
	portBindings := map[string]any{"5555/tcp": nil}
	if published {
		portBindings["5555/tcp"] = []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "49152"}}
	}
	value := []map[string]any{{
		"Name": "/" + containerName(accountID),
		"Config": map[string]any{
			"Image": image, "Hostname": containerHostname(accountID),
			"Labels": map[string]string{labelDriver: owner, labelAccount: accountID},
		},
		"State": map[string]bool{"Running": running},
		"HostConfig": map[string]any{
			"Privileged": true, "NetworkMode": networkName(accountID),
			"PublishAllPorts": false, "PortBindings": portBindings,
		},
		"NetworkSettings": map[string]any{
			"Ports": portBindings, "Networks": map[string]any{networkName(accountID): map[string]any{}},
		},
		"Mounts": []map[string]any{{
			"Type": "bind", "Source": dataDir, "Destination": "/data", "RW": true,
		}},
	}}
	contents, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func networkInspection(t *testing.T, accountID string) []byte {
	t.Helper()
	value := []map[string]any{{
		"Name": networkName(accountID), "Driver": "bridge", "Internal": false,
		"Attachable": false, "Ingress": false,
		"Labels": map[string]string{labelDriver: "wecom-network", labelAccount: accountID},
	}}
	contents, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func containsArgumentPair(arguments []string, key, value string) bool {
	for index := 0; index+1 < len(arguments); index++ {
		if reflect.DeepEqual(arguments[index:index+2], []string{key, value}) {
			return true
		}
	}
	return false
}
