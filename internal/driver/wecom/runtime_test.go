package wecom

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
	results  []executorResult
	calls    int
	commands []executorCall
}

type functionExecutor func(context.Context, string, ...string) ([]byte, error)

func (f functionExecutor) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

func (f functionExecutor) RunInput(ctx context.Context, _ []byte, _ int64, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

func (e *sequenceExecutor) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	e.commands = append(e.commands, executorCall{name: name, args: append([]string(nil), args...)})
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

func TestStoppedContainerExecutionEpochRejectsInvalidImmutableID(t *testing.T) {
	inspection := runtimeContainerInspection{ID: "not-an-immutable-container-id"}
	inspection.State.Status = "exited"
	inspection.State.StartedAt = "2026-08-16T12:00:00.000000000Z"
	inspection.State.FinishedAt = "2026-08-16T12:01:00.000000000Z"
	if _, err := stoppedContainerExecutionEpoch(inspection); err == nil {
		t.Fatal("stopped container epoch accepted an invalid immutable ID")
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
	runtime.containerID = testContainerID("container-work")
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

func TestStopTargetsPinnedIDWhenOriginalWasRenamedAndItsNameReused(t *testing.T) {
	config := validTestConfig(t)
	dataDir := t.TempDir()
	containerA := testContainerID("renamed-original")
	containerB := testContainerID("name-replacement")
	originalName := containerName("work")
	running := true
	var stopTargets []string
	executor := functionExecutor(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch {
		case len(args) == 3 && args[0] == "container" && args[1] == "inspect" && args[2] == containerA:
			return runtimeInspectionWithIDAndName(
				t, "renamed-outside-wechatcopilot", "wecom", "work", config.RedroidImage, dataDir, running, false, containerA,
			), nil
		case len(args) == 3 && args[0] == "container" && args[1] == "inspect" && args[2] == originalName:
			// This is the name-replacement container. A correct Stop never resolves
			// the mutable name after startup has pinned containerA.
			return runtimeInspectionWithIDAndName(
				t, originalName, "wecom", "work", config.RedroidImage, dataDir, true, false, containerB,
			), nil
		case len(args) == 5 && args[0] == "container" && args[1] == "stop":
			stopTargets = append(stopTargets, args[4])
			if args[4] != containerA {
				t.Fatalf("Stop targeted replacement container: %v", args)
			}
			running = false
			return []byte(containerA + "\n"), nil
		default:
			return nil, fmt.Errorf("unexpected Stop command: %v", args)
		}
	})
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
	runtime.containerName = originalName
	runtime.containerID = containerA
	runtime.networkName = networkName("work")
	runtime.lockFile = lock
	runtime.running = true

	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatalf("stop renamed pinned container with reused name: %v", err)
	}
	if !reflect.DeepEqual(stopTargets, []string{containerA}) {
		t.Fatalf("Stop targets = %v, want only original ID %s (replacement %s)", stopTargets, containerA, containerB)
	}
	if runtime.running || runtime.containerID != "" || runtime.lockFile != nil {
		t.Fatalf("successful pinned Stop retained runtime state: %#v", runtime)
	}
}

func TestStartExactContainerUsesPinnedIDAndRejectsNameReplacement(t *testing.T) {
	config := validTestConfig(t)
	dataDir := t.TempDir()
	containerA := testContainerID("start-container-a")
	containerB := testContainerID("start-container-b")
	executor := &sequenceExecutor{results: []executorResult{
		{output: runtimeInspectionWithID(t, "wecom", "work", config.RedroidImage, dataDir, false, false, containerA)},
		{output: []byte(containerA + "\n")},
		{output: runtimeInspectionWithID(t, "wecom", "work", config.RedroidImage, dataDir, true, false, containerB)},
	}}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	runtime.containerName = containerName("work")
	runtime.networkName = networkName("work")

	pinnedID, err := runtime.startExactStoppedContainer(context.Background(), "work", dataDir)
	if err == nil || !strings.Contains(err.Error(), "changed during start") {
		t.Fatalf("start replacement error = %v", err)
	}
	if pinnedID != containerA {
		t.Fatalf("pinned start ID = %q, want %q", pinnedID, containerA)
	}
	startArgs := executor.commands[1].args
	if !reflect.DeepEqual(startArgs, []string{"container", "start", containerA}) {
		t.Fatalf("start targeted a mutable name or replacement: %v", startArgs)
	}
}

func TestStopExactContainerUsesPinnedIDAndRejectsNameReplacement(t *testing.T) {
	config := validTestConfig(t)
	dataDir := t.TempDir()
	containerA := testContainerID("stop-container-a")
	containerB := testContainerID("stop-container-b")
	executor := &sequenceExecutor{results: []executorResult{
		{output: runtimeInspectionWithID(t, "wecom", "work", config.RedroidImage, dataDir, true, false, containerA)},
		{output: []byte(containerA + "\n")},
		{output: runtimeInspectionWithID(t, "wecom", "work", config.RedroidImage, dataDir, false, false, containerB)},
	}}
	runtime, err := NewRuntime(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	runtime.containerName = containerName("work")
	runtime.networkName = networkName("work")

	pinnedID, err := runtime.stopExactRunningContainer(context.Background(), "work", dataDir, 7)
	if err == nil || !strings.Contains(err.Error(), "changed during stop") {
		t.Fatalf("stop replacement error = %v", err)
	}
	if pinnedID != containerA {
		t.Fatalf("pinned stop ID = %q, want %q", pinnedID, containerA)
	}
	stopArgs := executor.commands[1].args
	if !reflect.DeepEqual(stopArgs, []string{"container", "stop", "--time", "7", containerA}) {
		t.Fatalf("stop targeted a mutable name or replacement: %v", stopArgs)
	}
}

func TestFailedStartCleanupUsesOnlyPinnedImmutableID(t *testing.T) {
	executor := &recordingExecutor{}
	runtime := &Runtime{config: Config{DockerBinary: "docker"}, executor: executor}
	containerID := testContainerID("failed-start-container")
	runtime.cleanupFailedStart(context.Background(), containerID)
	if len(executor.calls) != 1 || !reflect.DeepEqual(
		executor.calls[0].args,
		[]string{"container", "stop", "--time", "5", containerID},
	) {
		t.Fatalf("failed-start cleanup targeted a mutable name: %#v", executor.calls)
	}
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

func TestPurgeRefusesAnAccountWithAnActiveRuntimeLock(t *testing.T) {
	config := validTestConfig(t)
	stateDir := testAccountStateDir(t, "work")
	dataDir, err := accountDataDir(stateDir, "work")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dataDir, "session-state")
	if err := os.WriteFile(statePath, []byte("must remain"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireAccountLock(filepath.Join(filepath.Dir(dataDir), ".runtime.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer releaseAccountLock(lock)

	executor := &recordingExecutor{}
	driver, err := New(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Purge(context.Background(), core.AccountRuntime{AccountID: "work", StateDir: stateDir}); err == nil {
		t.Fatal("purge accepted an account whose cross-process runtime lock is held")
	}
	if len(executor.calls) != 0 {
		t.Fatalf("purge contacted Docker while the account runtime lock was held: %#v", executor.calls)
	}
	contents, err := os.ReadFile(statePath)
	if err != nil || string(contents) != "must remain" {
		t.Fatalf("purge changed account data while the runtime lock was held: %q, %v", contents, err)
	}
}

func TestPurgeRefusesForeignContainerLabels(t *testing.T) {
	config := validTestConfig(t)
	stateDir := testAccountStateDir(t, "work")
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
	if _, err := verifyPurgeContainer(raw, containerName("work"), "work", config.RedroidImage, expected); err == nil {
		t.Fatal("expected mismatched /data bind source to be rejected")
	}
}

func TestPurgeRemovesExactInactiveContainer(t *testing.T) {
	config := validTestConfig(t)
	stateDir := testAccountStateDir(t, "work")
	dataDir, err := accountDataDir(stateDir, "work")
	if err != nil {
		t.Fatal(err)
	}
	containerRemoved := false
	networkRemoved := false
	executor := functionExecutor(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch {
		case len(args) == 3 && args[0] == "container" && args[1] == "inspect":
			if containerRemoved {
				return nil, errors.New("container not found")
			}
			return purgeInspection(t, containerName("work"), "wecom", "work", config.RedroidImage, dataDir, false), nil
		case len(args) >= 2 && args[0] == "container" && args[1] == "ls":
			return nil, nil
		case len(args) == 3 && args[0] == "container" && args[1] == "rm":
			if args[2] != testContainerID("container-work") {
				t.Fatalf("purge removed container by mutable name: %v", args)
			}
			containerRemoved = true
			return nil, nil
		case len(args) == 3 && args[0] == "network" && args[1] == "inspect":
			if networkRemoved {
				return nil, errors.New("network not found")
			}
			return networkInspection(t, "work"), nil
		case len(args) >= 2 && args[0] == "network" && args[1] == "ls":
			return nil, nil
		case len(args) == 3 && args[0] == "network" && args[1] == "rm":
			if args[2] != testNetworkID("work") {
				t.Fatalf("purge removed network by mutable name: %v", args)
			}
			networkRemoved = true
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected purge command: %v", args)
		}
	})
	driver, err := New(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Purge(context.Background(), core.AccountRuntime{AccountID: "work", StateDir: stateDir}); err != nil {
		t.Fatal(err)
	}
	if !containerRemoved || !networkRemoved {
		t.Fatalf("exact purge did not remove both immutable objects: container=%v network=%v", containerRemoved, networkRemoved)
	}
}

func TestPurgeClearsContainerOwnedTreeBeforeCoreRemoval(t *testing.T) {
	config := validTestConfig(t)
	stateDir := testAccountStateDir(t, "work")
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
	containerRemoved := false
	networkRemoved := false
	executor := functionExecutor(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch {
		case len(args) == 3 && args[0] == "network" && args[1] == "inspect":
			if networkRemoved {
				return nil, errors.New("network not found")
			}
			return networkInspection(t, "work"), nil
		case len(args) >= 2 && args[0] == "network" && args[1] == "ls":
			return nil, nil
		case len(args) == 3 && args[0] == "network" && args[1] == "rm" && args[2] == testNetworkID("work"):
			networkRemoveChecked = true
			networkRemoved = true
			return nil, nil
		case len(args) >= 3 && args[0] == "container" && args[1] == "inspect":
			if containerRemoved {
				return nil, errors.New("container not found")
			}
			return purgeInspection(t, containerName("work"), "wecom", "work", config.RedroidImage, dataDir, false), nil
		case len(args) >= 2 && args[0] == "container" && args[1] == "ls":
			return nil, nil
		case len(args) >= 3 && args[0] == "container" && args[1] == "run":
			cleanupChecked = true
			if !containsArgumentPair(args, "--network", "none") ||
				!containsArgumentPair(args, "--entrypoint", "/system/bin/toybox") ||
				!containsArgumentPair(args, "--mount", "type=bind,src="+dataDir+",dst=/account-data") {
				t.Fatalf("cleanup container is not sufficiently constrained: %v", args)
			}
			dataInfo, err := os.Stat(dataDir)
			if err != nil {
				return nil, err
			}
			device, inode, err := directoryIdentity(dataInfo)
			if err != nil {
				return nil, err
			}
			wantTail := []string{"wechatcopilot-purge", strconv.FormatUint(device, 10), strconv.FormatUint(inode, 10)}
			if len(args) < len(wantTail) || !reflect.DeepEqual(args[len(args)-len(wantTail):], wantTail) {
				t.Fatalf("cleanup helper did not receive pinned inode: %v", args)
			}
			if err := os.RemoveAll(lockedDir); err != nil {
				return nil, err
			}
			return nil, nil
		case len(args) == 3 && args[0] == "container" && args[1] == "rm" && args[2] == testContainerID("container-work"):
			removeChecked = true
			containerRemoved = true
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
	entries, err := os.ReadDir(dataDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("expected pinned Android data to remain empty for account-store removal: entries=%v err=%v", entries, err)
	}
}

func TestPurgeCleanupRefusesExchangedDataTarget(t *testing.T) {
	config := validTestConfig(t)
	stateDir := testAccountStateDir(t, "work")
	dataDir, err := accountDataDir(stateDir, "work")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	authorizedState := filepath.Join(dataDir, "authorized-state")
	if err := os.WriteFile(authorizedState, []byte("authorized"), 0o600); err != nil {
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
	original := dataDir + ".approved"
	cleanupCalls := 0
	executor := functionExecutor(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch {
		case len(args) >= 2 && args[0] == "network" && args[1] == "inspect":
			return nil, errors.New("network not found")
		case len(args) >= 2 && args[0] == "network" && args[1] == "ls":
			return nil, nil
		case len(args) >= 2 && args[0] == "container" && args[1] == "inspect":
			return nil, errors.New("container not found")
		case len(args) >= 2 && args[0] == "container" && args[1] == "ls":
			return nil, nil
		case len(args) >= 2 && args[0] == "container" && args[1] == "run":
			cleanupCalls++
			wantTail := []string{"wechatcopilot-purge", strconv.FormatUint(device, 10), strconv.FormatUint(inode, 10)}
			if len(args) < len(wantTail) || !reflect.DeepEqual(args[len(args)-len(wantTail):], wantTail) {
				t.Fatalf("purge helper was not bound to the approved inode: %v", args)
			}
			if err := os.Rename(dataDir, original); err != nil {
				return nil, err
			}
			if err := os.Mkdir(dataDir, 0o700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(dataDir, "foreign-state"), []byte("foreign"), 0o600); err != nil {
				return nil, err
			}
			// The real helper exits before rm when its mounted inode differs.
			return nil, errors.New("exit status 72")
		case len(args) >= 2 && ((args[0] == "container" && args[1] == "rm") || (args[0] == "network" && args[1] == "rm")):
			t.Fatalf("purge continued to object removal after data target exchange: %v", args)
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected purge command: %v", args)
		}
	})
	driver, err := New(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	err = driver.Purge(context.Background(), core.AccountRuntime{AccountID: "work", StateDir: stateDir})
	if err == nil || !strings.Contains(err.Error(), "restricted cleanup") {
		t.Fatalf("exchanged purge target error = %v", err)
	}
	if cleanupCalls != 1 {
		t.Fatalf("purge cleanup calls = %d, want 1", cleanupCalls)
	}
	if contents, err := os.ReadFile(filepath.Join(original, "authorized-state")); err != nil || string(contents) != "authorized" {
		t.Fatalf("exchanged purge changed authorized inode: contents=%q err=%v", contents, err)
	}
	if contents, err := os.ReadFile(filepath.Join(dataDir, "foreign-state")); err != nil || string(contents) != "foreign" {
		t.Fatalf("exchanged purge changed foreign replacement: contents=%q err=%v", contents, err)
	}
}

func TestPurgeRejectsCanonicalDataReplacementAfterPinnedClear(t *testing.T) {
	config := validTestConfig(t)
	stateDir := testAccountStateDir(t, "work")
	dataDir, err := accountDataDir(stateDir, "work")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	authorizedState := filepath.Join(dataDir, "authorized-state")
	if err := os.WriteFile(authorizedState, []byte("authorized"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := dataDir + ".cleared"
	executor := functionExecutor(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch {
		case len(args) >= 2 && args[0] == "network" && args[1] == "inspect":
			return nil, errors.New("network not found")
		case len(args) >= 2 && args[0] == "network" && args[1] == "ls":
			return nil, nil
		case len(args) >= 2 && args[0] == "container" && args[1] == "inspect":
			return nil, errors.New("container not found")
		case len(args) >= 2 && args[0] == "container" && args[1] == "ls":
			return nil, nil
		case len(args) >= 2 && args[0] == "container" && args[1] == "run":
			if err := os.Remove(authorizedState); err != nil {
				return nil, err
			}
			if err := os.Rename(dataDir, original); err != nil {
				return nil, err
			}
			if err := os.Mkdir(dataDir, 0o700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(dataDir, "foreign-state"), []byte("foreign"), 0o600); err != nil {
				return nil, err
			}
			return nil, nil
		case len(args) >= 2 && ((args[0] == "container" && args[1] == "rm") || (args[0] == "network" && args[1] == "rm")):
			t.Fatalf("purge removed Docker object after canonical data replacement: %v", args)
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected purge command: %v", args)
		}
	})
	driver, err := New(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	err = driver.Purge(context.Background(), core.AccountRuntime{AccountID: "work", StateDir: stateDir})
	if err == nil || !strings.Contains(err.Error(), "changed during purge") {
		t.Fatalf("post-clear canonical replacement error = %v", err)
	}
	if entries, err := os.ReadDir(original); err != nil || len(entries) != 0 {
		t.Fatalf("pinned intended directory was not the cleared inode: entries=%v err=%v", entries, err)
	}
	if contents, err := os.ReadFile(filepath.Join(dataDir, "foreign-state")); err != nil || string(contents) != "foreign" {
		t.Fatalf("post-clear purge changed foreign replacement: contents=%q err=%v", contents, err)
	}
}

func TestPurgeContainerNameReplacementRemovesOnlyPinnedID(t *testing.T) {
	config := validTestConfig(t)
	stateDir := testAccountStateDir(t, "work")
	dataDir, err := accountDataDir(stateDir, "work")
	if err != nil {
		t.Fatal(err)
	}
	containerA := testContainerID("purge-container-a")
	containerB := testContainerID("purge-container-b")
	inspectCalls := 0
	var removed []string
	executor := functionExecutor(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch {
		case len(args) >= 2 && args[0] == "network" && args[1] == "inspect":
			return nil, errors.New("network not found")
		case len(args) >= 2 && args[0] == "network" && args[1] == "ls":
			return nil, nil
		case len(args) == 3 && args[0] == "container" && args[1] == "inspect":
			inspectCalls++
			if inspectCalls <= 2 {
				return runtimeInspectionWithID(t, "wecom", "work", config.RedroidImage, dataDir, false, false, containerA), nil
			}
			return runtimeInspectionWithID(t, "foreign", "work", config.RedroidImage, dataDir, false, false, containerB), nil
		case len(args) == 3 && args[0] == "container" && args[1] == "rm":
			removed = append(removed, args[2])
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected purge command: %v", args)
		}
	})
	driver, err := New(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	err = driver.Purge(context.Background(), core.AccountRuntime{AccountID: "work", StateDir: stateDir})
	if err == nil || !strings.Contains(err.Error(), "after purge removal") {
		t.Fatalf("container name replacement error = %v", err)
	}
	if !reflect.DeepEqual(removed, []string{containerA}) {
		t.Fatalf("purge removed replacement container: removed=%v replacement=%s", removed, containerB)
	}
}

func TestPurgeNetworkNameReplacementRemovesOnlyPinnedID(t *testing.T) {
	config := validTestConfig(t)
	stateDir := testAccountStateDir(t, "work")
	networkA := testContainerID("purge-network-a")
	networkB := testContainerID("purge-network-b")
	networkRemoved := false
	var removed []string
	executor := functionExecutor(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch {
		case len(args) == 3 && args[0] == "network" && args[1] == "inspect":
			if networkRemoved {
				return networkInspectionWithID(t, "work", networkB, "foreign-network"), nil
			}
			return networkInspectionWithID(t, "work", networkA, "wecom-network"), nil
		case len(args) == 3 && args[0] == "network" && args[1] == "rm":
			removed = append(removed, args[2])
			networkRemoved = true
			return nil, nil
		case len(args) >= 2 && args[0] == "container" && args[1] == "inspect":
			return nil, errors.New("container not found")
		case len(args) >= 2 && args[0] == "container" && args[1] == "ls":
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected purge command: %v", args)
		}
	})
	driver, err := New(config, executor)
	if err != nil {
		t.Fatal(err)
	}
	err = driver.Purge(context.Background(), core.AccountRuntime{AccountID: "work", StateDir: stateDir})
	if err == nil || !strings.Contains(err.Error(), "after removal") {
		t.Fatalf("network name replacement error = %v", err)
	}
	if !reflect.DeepEqual(removed, []string{networkA}) {
		t.Fatalf("purge removed replacement network: removed=%v replacement=%s", removed, networkB)
	}
}

func TestPurgeCleanupHelperChecksMountedInodeBeforeDelete(t *testing.T) {
	image := "registry.example/redroid@sha256:" + strings.Repeat("a", 64)
	args := weComPurgeCleanupArgs(image, "/private/account/data", "work", 41, 42)
	for _, pair := range [][2]string{
		{"--pull", "never"}, {"--network", "none"}, {"--cap-drop", "ALL"},
		{"--cap-add", "DAC_OVERRIDE"}, {"--cap-add", "FOWNER"},
		{"--security-opt", "no-new-privileges=true"}, {"--user", "0:0"},
		{"--entrypoint", "/system/bin/toybox"},
		{"--mount", "type=bind,src=/private/account/data,dst=/account-data"},
	} {
		if !containsArgumentPair(args, pair[0], pair[1]) {
			t.Fatalf("purge helper lacks %q %q: %v", pair[0], pair[1], args)
		}
	}
	joined := strings.Join(args, "\n")
	statIndex := strings.Index(joined, "/system/bin/toybox stat -Lc %d:%i /account-data")
	rmIndex := strings.Index(joined, "/system/bin/toybox rm -rf /account-data/")
	if statIndex < 0 || rmIndex < 0 || statIndex >= rmIndex {
		t.Fatalf("purge helper does not verify its mounted inode before deletion: %v", args)
	}
	wantTail := []string{"wechatcopilot-purge", "41", "42"}
	if !reflect.DeepEqual(args[len(args)-len(wantTail):], wantTail) {
		t.Fatalf("purge helper identity tail = %v, want %v", args, wantTail)
	}
}

func testAccountStateDir(t *testing.T, accountID string) string {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), accountID)
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return stateDir
}

func purgeInspection(t *testing.T, name, owner, accountID, image, dataDir string, running bool) []byte {
	t.Helper()
	epoch := testContainerExecutionEpoch(accountID)
	if running {
		epoch.Status = "running"
		epoch.FinishedAt = "0001-01-01T00:00:00Z"
	}
	value := []map[string]any{{
		"Id": testContainerID("container-" + accountID), "Name": "/" + name,
		"RestartCount": epoch.RestartCount,
		"Config": map[string]any{
			"Image": image, "Hostname": containerHostname(accountID),
			"Labels": map[string]string{labelDriver: owner, labelAccount: accountID},
		},
		"State": map[string]any{
			"Status": epoch.Status, "Running": running, "Paused": false, "Restarting": false,
			"ExitCode": epoch.ExitCode, "StartedAt": epoch.StartedAt, "FinishedAt": epoch.FinishedAt,
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

func runtimeInspection(t *testing.T, owner, accountID, image, dataDir string, running, published bool) []byte {
	return runtimeInspectionWithID(t, owner, accountID, image, dataDir, running, published, testContainerID("container-"+accountID))
}

func runtimeInspectionWithID(t *testing.T, owner, accountID, image, dataDir string, running, published bool, id string) []byte {
	return runtimeInspectionWithIDAndName(
		t, containerName(accountID), owner, accountID, image, dataDir, running, published, id,
	)
}

func runtimeInspectionWithIDAndName(t *testing.T, name, owner, accountID, image, dataDir string, running, published bool, id string) []byte {
	t.Helper()
	if !validImmutableContainerID(id) {
		id = testContainerID(id)
	}
	epoch := testContainerExecutionEpochWithID(id)
	if running {
		epoch.Status = "running"
		epoch.FinishedAt = "0001-01-01T00:00:00Z"
	}
	portBindings := map[string]any{"5555/tcp": nil}
	if published {
		portBindings["5555/tcp"] = []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "49152"}}
	}
	value := []map[string]any{{
		"Id":           id,
		"Name":         "/" + name,
		"RestartCount": epoch.RestartCount,
		"Config": map[string]any{
			"Image": image, "Hostname": containerHostname(accountID),
			"Labels": map[string]string{labelDriver: owner, labelAccount: accountID},
		},
		"State": map[string]any{
			"Status": epoch.Status, "Running": running, "Paused": false, "Restarting": false,
			"ExitCode": epoch.ExitCode, "StartedAt": epoch.StartedAt, "FinishedAt": epoch.FinishedAt,
		},
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

func testContainerID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

func testContainerExecutionEpoch(accountID string) containerExecutionEpoch {
	return testContainerExecutionEpochWithID(testContainerID("container-" + accountID))
}

func testContainerExecutionEpochWithID(id string) containerExecutionEpoch {
	if !validImmutableContainerID(id) {
		id = testContainerID(id)
	}
	return containerExecutionEpoch{
		ID:           id,
		Status:       "exited",
		StartedAt:    "2026-08-16T12:00:00.000000000Z",
		FinishedAt:   "2026-08-16T12:01:00.000000000Z",
		RestartCount: 0,
		ExitCode:     0,
	}
}

func networkInspection(t *testing.T, accountID string) []byte {
	return networkInspectionWithID(t, accountID, testNetworkID(accountID), "wecom-network")
}

func networkInspectionWithID(t *testing.T, accountID, id, owner string) []byte {
	t.Helper()
	value := []map[string]any{{
		"Id": id, "Name": networkName(accountID), "Driver": "bridge", "Internal": false,
		"Attachable": false, "Ingress": false,
		"Labels": map[string]string{labelDriver: owner, labelAccount: accountID},
	}}
	contents, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func testNetworkID(accountID string) string {
	return testContainerID("network:" + accountID)
}

func containsArgumentPair(arguments []string, key, value string) bool {
	for index := 0; index+1 < len(arguments); index++ {
		if reflect.DeepEqual(arguments[index:index+2], []string{key, value}) {
			return true
		}
	}
	return false
}
