package wechat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	shared "github.com/gih10012/wechatcopilot/internal/driver"
)

type dockerFixtureRunner struct {
	commands               []Command
	image                  runtimeImageInspection
	container              *containerInspection
	beforeContainerInspect func() error
	execResponse           []byte
}

func (r *dockerFixtureRunner) Run(_ context.Context, command Command) ([]byte, error) {
	r.commands = append(r.commands, command)
	if len(command.Args) >= 2 && command.Args[0] == "image" && command.Args[1] == "inspect" {
		return mustMarshalFixture([]runtimeImageInspection{r.image}), nil
	}
	if len(command.Args) >= 2 && command.Args[0] == "container" && command.Args[1] == "inspect" {
		if r.beforeContainerInspect != nil {
			if err := r.beforeContainerInspect(); err != nil {
				return nil, err
			}
		}
		if r.container == nil {
			return nil, errors.New("fixture: no container")
		}
		return mustMarshalFixture([]containerInspection{*r.container}), nil
	}
	if len(command.Args) >= 2 && command.Args[0] == "container" && command.Args[1] == "ls" {
		if r.container == nil {
			return nil, nil
		}
		return []byte("fixture-container-id\n"), nil
	}
	if len(command.Args) > 0 && command.Args[0] == "exec" {
		if r.execResponse != nil {
			return r.execResponse, nil
		}
		return []byte(`{"ok":true}`), nil
	}
	return []byte("fixture-container-id\n"), nil
}

func mustMarshalFixture(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func fixtureRuntimeImage() runtimeImageInspection {
	var image runtimeImageInspection
	image.ID = "sha256:" + strings.Repeat("a", 64)
	image.Config.Entrypoint = []string{"/opt/wechatcopilot/entrypoint"}
	image.Config.Cmd = []string{"/bin/bash"}
	image.Config.WorkingDir = "/home/wechat"
	image.Config.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	image.Config.Labels = map[string]string{"fixture.runtime": "wechat"}
	return image
}

func TestDockerBackendUsesOnlyIsolatedProfileMounts(t *testing.T) {
	temporary := t.TempDir()
	appImage := filepath.Join(temporary, "WeChat.AppImage")
	digest := writeAppImageFixture(t, appImage, "fixture")
	profile, err := (ProfileManager{}).Ensure(sharedAccountFixture(temporary))
	if err != nil {
		t.Fatal(err)
	}
	runner := &dockerFixtureRunner{image: fixtureRuntimeImage()}
	backend, err := NewDockerBackend(DockerConfig{
		Image: "wechatcopilot/wechat-runtime:test", AppImagePath: appImage,
		ExpectedAppImageSHA256: digest, Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Start(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	var run Command
	for _, command := range runner.commands {
		if len(command.Args) > 0 && command.Args[0] == "run" {
			run = command
			break
		}
	}
	if len(run.Args) == 0 {
		t.Fatal("docker run was not invoked")
	}
	if run.Args[len(run.Args)-1] != runner.image.ID {
		t.Fatalf("docker run did not use immutable image ID: %#v", run.Args)
	}
	joined := strings.Join(run.Args, "\n")
	for _, required := range []string{profile.ClientHome + ":/home/wechat:rw", profile.Files, profile.Runtime, appImage} {
		if !strings.Contains(joined, required) {
			t.Errorf("docker run does not contain isolated mount %q", required)
		}
	}
	if home, err := os.UserHomeDir(); err == nil && strings.Contains(joined, filepath.Join(home, ".xwechat")) {
		t.Fatal("docker run mounted the operator's current WeChat profile")
	}
	if err := backend.SubmitAuthCode(context.Background(), "123456"); err != nil {
		t.Fatal(err)
	}
	last := runner.commands[len(runner.commands)-1]
	if strings.Contains(strings.Join(last.Args, " "), "123456") {
		t.Fatal("authentication code leaked into process arguments")
	}
	if !strings.Contains(string(last.Stdin), "123456") {
		t.Fatal("authentication code was not passed through stdin")
	}
}

func TestDockerBackendParsesOnlyFixedSavedAccountAuthActions(t *testing.T) {
	runner := &dockerFixtureRunner{}
	backend := activeDockerControlFixture(t, runner)
	generation := strings.Repeat("a", 64)
	screenshot := testSurfacePNG(t, 8, 6)
	digest := sha256.Sum256(screenshot)
	screenshotSHA256 := hex.EncodeToString(digest[:])
	runner.execResponse = mustMarshalFixture(map[string]any{
		"ok": true, "state": "AUTH_REQUIRED", "auth_kind": "phone_confirmation",
		"prompt": "Confirm saved account", "auth_generation": generation,
		"screenshot_base64": base64.StdEncoding.EncodeToString(screenshot), "screenshot_sha256": screenshotSHA256,
		"actions": []map[string]any{
			{
				"id": savedAccountLoginActionPrefix + generation, "label": "runtime-controlled label",
				"risk": "low", "confirmation": "runtime-controlled confirmation",
				"requires_confirmation": true, "image_bound": true,
			},
			{
				"id": savedAccountSwitchActionPrefix + generation, "label": "untrusted switch label",
				"risk": "low", "confirmation": "untrusted switch confirmation",
				"requires_confirmation": true, "image_bound": true,
			},
		},
	})
	probe, err := backend.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []shared.AuthAction{
		savedAccountLoginAction(generation), savedAccountSwitchAction(generation),
	}
	if len(probe.Actions) != len(want) || probe.Actions[0] != want[0] || probe.Actions[1] != want[1] {
		t.Fatalf("canonical authentication actions = %#v, want %#v", probe.Actions, want)
	}
	if !bytes.Equal(probe.ScreenshotPNG, screenshot) {
		t.Fatal("backend did not preserve the screenshot bound to the authentication action")
	}

	invalid := []map[string]any{
		{
			"ok": true, "state": "ONLINE", "auth_kind": "phone_confirmation",
			"auth_generation": generation, "screenshot_base64": base64.StdEncoding.EncodeToString(screenshot),
			"screenshot_sha256": screenshotSHA256,
			"actions":           []map[string]any{{"id": savedAccountLoginActionPrefix + generation, "requires_confirmation": true, "image_bound": true}},
		},
		{
			"ok": true, "state": "AUTH_REQUIRED", "auth_kind": "phone_confirmation",
			"auth_generation": generation, "screenshot_base64": base64.StdEncoding.EncodeToString(screenshot),
			"screenshot_sha256": screenshotSHA256,
			"actions":           []map[string]any{{"id": "arbitrary_action", "requires_confirmation": true, "image_bound": true}},
		},
		{
			"ok": true, "state": "AUTH_REQUIRED", "auth_kind": "phone_confirmation",
			"auth_generation": generation, "screenshot_base64": base64.StdEncoding.EncodeToString(screenshot),
			"screenshot_sha256": screenshotSHA256,
			"actions":           []map[string]any{{"id": savedAccountSwitchActionPrefix + generation, "requires_confirmation": true, "image_bound": true}},
		},
		{
			"ok": true, "state": "AUTH_REQUIRED", "auth_kind": "phone_confirmation",
			"auth_generation": generation, "screenshot_base64": base64.StdEncoding.EncodeToString(screenshot),
			"screenshot_sha256": screenshotSHA256,
			"actions":           []map[string]any{{"id": savedAccountLoginActionPrefix + generation, "requires_confirmation": true}},
		},
		{
			"ok": true, "state": "AUTH_REQUIRED", "auth_kind": "phone_confirmation",
			"auth_generation": generation, "screenshot_base64": base64.StdEncoding.EncodeToString(screenshot),
			"screenshot_sha256": strings.Repeat("f", 64),
			"actions":           []map[string]any{{"id": savedAccountLoginActionPrefix + generation, "requires_confirmation": true, "image_bound": true}},
		},
		{
			"ok": true, "state": "AUTH_REQUIRED", "auth_kind": "phone_confirmation",
			"auth_generation": generation, "screenshot_base64": base64.StdEncoding.EncodeToString(screenshot),
			"screenshot_sha256": screenshotSHA256,
		},
	}
	for index, response := range invalid {
		runner.execResponse = mustMarshalFixture(response)
		if _, err := backend.Probe(context.Background()); !errors.Is(err, ErrClientIncompatible) {
			t.Fatalf("invalid authentication action response %d error = %v, want ErrClientIncompatible", index, err)
		}
	}
}

func TestDockerBackendSavedAccountLoginUsesFixedLocatorFreeOperation(t *testing.T) {
	runner := &dockerFixtureRunner{execResponse: []byte(`{"ok":true,"consumed":true}`)}
	backend := activeDockerControlFixture(t, runner)
	generation := strings.Repeat("b", 64)
	if err := backend.ContinueSavedAccountLogin(context.Background(), generation); err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 1 {
		t.Fatalf("authentication action commands = %d, want 1", len(runner.commands))
	}
	var request map[string]any
	if err := json.Unmarshal(runner.commands[0].Stdin, &request); err != nil {
		t.Fatal(err)
	}
	if len(request) != 2 || request["operation"] != continueSavedAccountLoginOperation ||
		request["expected_auth_generation"] != generation {
		t.Fatalf("authentication control request must contain only its fixed operation and opaque generation: %#v", request)
	}

	if err := backend.SwitchSavedAccountLogin(context.Background(), generation); err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 2 {
		t.Fatalf("authentication action commands = %d, want 2", len(runner.commands))
	}
	request = nil
	if err := json.Unmarshal(runner.commands[1].Stdin, &request); err != nil {
		t.Fatal(err)
	}
	if len(request) != 2 || request["operation"] != switchSavedAccountLoginOperation ||
		request["expected_auth_generation"] != generation {
		t.Fatalf("switch authentication request must contain only its fixed operation and opaque generation: %#v", request)
	}

	runner.execResponse = []byte(`{"ok":false,"code":"ACTION_OUTCOME_UNCERTAIN","error":"response lost after activation","consumed":true}`)
	err := backend.ContinueSavedAccountLogin(context.Background(), generation)
	if err == nil || !shared.AuthActionWasConsumed(err) {
		t.Fatalf("consumed control failure = %v, want consumed marker", err)
	}
	commands := len(runner.commands)
	if err := backend.ContinueSavedAccountLogin(context.Background(), strings.Repeat("A", 64)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid authentication generation error = %v, want ErrInvalidArgument", err)
	}
	if len(runner.commands) != commands {
		t.Fatal("invalid authentication generation reached the control process")
	}
}

func activeDockerControlFixture(t *testing.T, runner CommandRunner) *DockerBackend {
	t.Helper()
	backend, err := NewDockerBackend(DockerConfig{
		Image:                  "wechatcopilot/wechat-runtime:test",
		AppImagePath:           filepath.Join(t.TempDir(), "unused.AppImage"),
		ExpectedAppImageSHA256: strings.Repeat("0", 64),
		Runner:                 runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	backend.profile = &Profile{}
	backend.container = "wechatcopilot-wechat-fixture"
	return backend
}

func writeAppImageFixture(t *testing.T, path, payload string) string {
	t.Helper()
	contents := append([]byte{0x7f, 'E', 'L', 'F'}, []byte(payload)...)
	if err := os.WriteFile(path, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func sharedAccountFixture(root string) shared.AccountRuntime {
	return shared.AccountRuntime{
		AccountID: "wx-main", Alias: "Main",
		StateDir: filepath.Join(root, "state"), RuntimeDir: filepath.Join(root, "runtime"),
	}
}

func purgeInspection(accountID, stateRoot string, running bool) containerInspection {
	var inspection containerInspection
	inspection.Name = "/" + containerName(accountID)
	inspection.State.Running = running
	inspection.Config.Labels = map[string]string{
		"io.wechatcopilot.driver":  "wechat-linux",
		"io.wechatcopilot.account": accountID,
		"io.wechatcopilot.profile": fingerprint(stateRoot),
	}
	return inspection
}

func validContainerInspection(backend *DockerBackend, image runtimeImageInspection, profile Profile, clientFingerprint string) containerInspection {
	var inspection containerInspection
	inspection.Name = "/" + containerName(profile.AccountID)
	inspection.Image = image.ID
	inspection.Config.Image = image.ID
	inspection.Config.Hostname = profile.Hostname
	inspection.Config.User = strconv.Itoa(backend.config.UID) + ":" + strconv.Itoa(backend.config.GID)
	inspection.Config.WorkingDir = image.Config.WorkingDir
	inspection.Config.Entrypoint = append([]string(nil), image.Config.Entrypoint...)
	inspection.Config.Cmd = append([]string(nil), image.Config.Cmd...)
	inspection.Config.Env = append(append([]string(nil), image.Config.Env...),
		"WECHAT_DISPLAY="+backend.config.Display,
		"XDG_RUNTIME_DIR=/wechatcopilot/runtime/xdg",
	)
	inspection.Config.Labels = cloneStrings(image.Config.Labels)
	inspection.Config.Labels["io.wechatcopilot.driver"] = "wechat-linux"
	inspection.Config.Labels["io.wechatcopilot.account"] = profile.AccountID
	inspection.Config.Labels["io.wechatcopilot.profile"] = fingerprint(profile.Root)
	inspection.Config.Labels["io.wechatcopilot.client"] = clientFingerprint
	inspection.Config.Labels["io.wechatcopilot.runtime"] = fingerprint(image.ID)
	inspection.Config.Labels["io.wechatcopilot.config"] = backend.containerConfigFingerprint(profile, image.ID)
	inspection.Config.AttachStdout = true
	inspection.Config.AttachStderr = true
	inspection.State.Running = true
	inspection.HostConfig.NetworkMode = "bridge"
	inspection.HostConfig.CapDrop = []string{"ALL"}
	inspection.HostConfig.SecurityOpt = []string{"no-new-privileges"}
	inspection.HostConfig.PidsLimit = 2048
	inspection.HostConfig.Memory = backend.memoryBytes
	inspection.HostConfig.MemorySwap = backend.memoryBytes
	inspection.HostConfig.ShmSize = backend.shmSizeBytes
	inspection.HostConfig.Tmpfs = map[string]string{"/tmp": "rw,nosuid,nodev,exec,mode=1777"}
	inspection.HostConfig.RestartPolicy.Name = "no"
	inspection.NetworkSettings.Networks = map[string]json.RawMessage{"bridge": json.RawMessage(`{}`)}
	inspection.Mounts = []containerMount{
		{Type: "bind", Source: backend.config.AppImagePath, Destination: "/opt/wechat/WeChat.AppImage", RW: false, Propagation: "rprivate"},
		{Type: "bind", Source: profile.ClientHome, Destination: "/home/wechat", RW: true, Propagation: "rprivate"},
		{Type: "bind", Source: profile.Files, Destination: "/home/wechat/WeChat_Files", RW: true, Propagation: "rprivate"},
		{Type: "bind", Source: profile.Runtime, Destination: "/wechatcopilot/runtime", RW: true, Propagation: "rprivate"},
		{Type: "bind", Source: filepath.Join(profile.Root, "machine-id"), Destination: "/etc/machine-id", RW: false, Propagation: "rprivate"},
	}
	return inspection
}

func TestDockerPurgeDoesNotNeedClientAndChecksOwnership(t *testing.T) {
	temporary := t.TempDir()
	account := sharedAccountFixture(temporary)
	stateRoot, err := cleanAbsolute(account.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	inspection := purgeInspection(account.AccountID, stateRoot, false)
	runner := &dockerFixtureRunner{container: &inspection}
	backend, err := NewDockerBackend(DockerConfig{
		Image:                  "wechatcopilot/wechat-runtime:test",
		AppImagePath:           filepath.Join(temporary, "client-no-longer-exists.AppImage"),
		ExpectedAppImageSHA256: strings.Repeat("0", 64),
		Runner:                 runner,
	})
	if err != nil {
		t.Fatalf("purge backend unexpectedly required AppImage: %v", err)
	}
	if err := backend.Purge(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	last := runner.commands[len(runner.commands)-1]
	if len(last.Args) != 3 || last.Args[0] != "container" || last.Args[1] != "rm" || last.Args[2] != containerName(account.AccountID) {
		t.Fatalf("unexpected purge command: %#v", last)
	}
}

func TestDockerPurgeRefusesRunningOrMismatchedContainer(t *testing.T) {
	temporary := t.TempDir()
	account := sharedAccountFixture(temporary)
	stateRoot, err := cleanAbsolute(account.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*containerInspection)
	}{
		{name: "running", mutate: func(value *containerInspection) { value.State.Running = true }},
		{name: "wrong profile", mutate: func(value *containerInspection) {
			value.Config.Labels["io.wechatcopilot.profile"] = fingerprint(stateRoot + "-other")
		}},
		{name: "wrong driver", mutate: func(value *containerInspection) {
			value.Config.Labels["io.wechatcopilot.driver"] = "unrelated"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspection := purgeInspection(account.AccountID, stateRoot, false)
			test.mutate(&inspection)
			runner := &dockerFixtureRunner{container: &inspection}
			backend, err := NewDockerBackend(DockerConfig{
				Image:                  "wechatcopilot/wechat-runtime:test",
				AppImagePath:           filepath.Join(temporary, "missing.AppImage"),
				ExpectedAppImageSHA256: strings.Repeat("0", 64), Runner: runner,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.Purge(context.Background(), account); err == nil {
				t.Fatal("expected unsafe purge to be rejected")
			}
			for _, command := range runner.commands {
				if len(command.Args) > 1 && command.Args[0] == "container" && command.Args[1] == "rm" {
					t.Fatalf("unsafe container was removed: %#v", command)
				}
			}
		})
	}
}

func TestNewDockerBackendRequiresPinnedAppImageDigest(t *testing.T) {
	for _, digest := range []string{"", "not-a-digest", strings.Repeat("a", 63), strings.Repeat("z", 64)} {
		_, err := NewDockerBackend(DockerConfig{
			Image: "wechatcopilot/wechat-runtime:test", AppImagePath: filepath.Join(t.TempDir(), "WeChat.AppImage"),
			ExpectedAppImageSHA256: digest,
		})
		if err == nil {
			t.Fatalf("NewDockerBackend accepted invalid digest %q", digest)
		}
	}
}

func TestNewDockerBackendRejectsAmbiguousResourceSizes(t *testing.T) {
	for _, test := range []struct {
		memory string
		shm    string
	}{
		{memory: "4gb", shm: "1g"},
		{memory: "0g", shm: "1g"},
		{memory: "4g", shm: "1024"},
		{memory: "999999999999999999999g", shm: "1g"},
	} {
		_, err := NewDockerBackend(DockerConfig{
			Image: "wechatcopilot/wechat-runtime:test", AppImagePath: filepath.Join(t.TempDir(), "WeChat.AppImage"),
			ExpectedAppImageSHA256: strings.Repeat("a", 64), Memory: test.memory, SHMSize: test.shm,
		})
		if err == nil {
			t.Fatalf("NewDockerBackend accepted memory=%q shm=%q", test.memory, test.shm)
		}
	}
}

func TestDockerStartReusesOnlyExactIsolatedContainer(t *testing.T) {
	root := t.TempDir()
	appImage := filepath.Join(root, "WeChat.AppImage")
	digest := writeAppImageFixture(t, appImage, "fixture")
	profile, err := (ProfileManager{}).Ensure(sharedAccountFixture(root))
	if err != nil {
		t.Fatal(err)
	}
	image := fixtureRuntimeImage()
	runner := &dockerFixtureRunner{image: image}
	backend, err := NewDockerBackend(DockerConfig{
		Image: "wechatcopilot/wechat-runtime:mutable", AppImagePath: appImage,
		ExpectedAppImageSHA256: digest, Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	inspection := validContainerInspection(backend, image, profile, digest)
	runner.container = &inspection
	if err := backend.Start(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	for _, command := range runner.commands {
		if len(command.Args) > 0 && (command.Args[0] == "run" || command.Args[0] == "start") {
			t.Fatalf("an already-running exact container was relaunched: %#v", command)
		}
	}
}

func TestDockerStartRejectsMismatchedExistingContainer(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*containerInspection)
	}{
		{name: "wrong appimage mount", mutate: func(value *containerInspection) {
			value.Mounts[0].Source += "-other"
		}},
		{name: "extra host mount", mutate: func(value *containerInspection) {
			value.Mounts = append(value.Mounts, containerMount{Type: "bind", Source: "/", Destination: "/host", RW: true})
		}},
		{name: "shared mount propagation", mutate: func(value *containerInspection) {
			value.Mounts[1].Propagation = "rshared"
		}},
		{name: "published port", mutate: func(value *containerInspection) {
			value.HostConfig.PortBindings = map[string][]json.RawMessage{"8080/tcp": {json.RawMessage(`{"HostPort":"8080"}`)}}
		}},
		{name: "extra network", mutate: func(value *containerInspection) {
			value.NetworkSettings.Networks["other"] = json.RawMessage(`{}`)
		}},
		{name: "missing cap drop", mutate: func(value *containerInspection) {
			value.HostConfig.CapDrop = nil
		}},
		{name: "wrong memory limit", mutate: func(value *containerInspection) {
			value.HostConfig.Memory /= 2
		}},
		{name: "swap enabled", mutate: func(value *containerInspection) {
			value.HostConfig.MemorySwap *= 2
		}},
		{name: "wrong shared memory", mutate: func(value *containerInspection) {
			value.HostConfig.ShmSize /= 2
		}},
		{name: "unconfined security", mutate: func(value *containerInspection) {
			value.HostConfig.SecurityOpt = []string{"no-new-privileges", "seccomp=unconfined"}
		}},
		{name: "wrong user", mutate: func(value *containerInspection) {
			value.Config.User = "0:0"
		}},
		{name: "extra environment", mutate: func(value *containerInspection) {
			value.Config.Env = append(value.Config.Env, "LD_PRELOAD=/host/inject.so")
		}},
		{name: "runtime tag drift", mutate: func(value *containerInspection) {
			value.Image = "sha256:" + strings.Repeat("b", 64)
		}},
		{name: "missing config fingerprint", mutate: func(value *containerInspection) {
			delete(value.Config.Labels, "io.wechatcopilot.config")
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			appImage := filepath.Join(root, "WeChat.AppImage")
			digest := writeAppImageFixture(t, appImage, "fixture")
			profile, err := (ProfileManager{}).Ensure(sharedAccountFixture(root))
			if err != nil {
				t.Fatal(err)
			}
			image := fixtureRuntimeImage()
			runner := &dockerFixtureRunner{image: image}
			backend, err := NewDockerBackend(DockerConfig{
				Image: "wechatcopilot/wechat-runtime:mutable", AppImagePath: appImage,
				ExpectedAppImageSHA256: digest, Runner: runner,
			})
			if err != nil {
				t.Fatal(err)
			}
			inspection := validContainerInspection(backend, image, profile, digest)
			test.mutate(&inspection)
			runner.container = &inspection

			err = backend.Start(context.Background(), profile)
			if !errors.Is(err, ErrClientIncompatible) {
				t.Fatalf("Start error = %v, want ErrClientIncompatible", err)
			}
			for _, command := range runner.commands {
				if len(command.Args) > 0 && (command.Args[0] == "run" || command.Args[0] == "start" || command.Args[0] == "exec") {
					t.Fatalf("unsafe existing container was used: %#v", command)
				}
			}
		})
	}
}

func TestDockerStartRejectsUnexpectedRuntimeImage(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runtimeImageInspection)
	}{
		{name: "non immutable id", mutate: func(value *runtimeImageInspection) { value.ID = "mutable" }},
		{name: "wrong entrypoint", mutate: func(value *runtimeImageInspection) {
			value.Config.Entrypoint = []string{"/bin/sh"}
		}},
		{name: "published image port", mutate: func(value *runtimeImageInspection) {
			value.Config.ExposedPorts = map[string]json.RawMessage{"8080/tcp": json.RawMessage(`{}`)}
		}},
		{name: "anonymous image volume", mutate: func(value *runtimeImageInspection) {
			value.Config.Volumes = map[string]json.RawMessage{"/host": json.RawMessage(`{}`)}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			appImage := filepath.Join(root, "WeChat.AppImage")
			digest := writeAppImageFixture(t, appImage, "fixture")
			profile, err := (ProfileManager{}).Ensure(sharedAccountFixture(root))
			if err != nil {
				t.Fatal(err)
			}
			image := fixtureRuntimeImage()
			test.mutate(&image)
			runner := &dockerFixtureRunner{image: image}
			backend, err := NewDockerBackend(DockerConfig{
				Image: "wechatcopilot/wechat-runtime:test", AppImagePath: appImage,
				ExpectedAppImageSHA256: digest, Runner: runner,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.Start(context.Background(), profile); !errors.Is(err, ErrClientIncompatible) {
				t.Fatalf("Start error = %v, want ErrClientIncompatible", err)
			}
			for _, command := range runner.commands {
				if len(command.Args) > 0 && (command.Args[0] == "run" || command.Args[0] == "start") {
					t.Fatalf("unexpected runtime image was launched: %#v", command)
				}
			}
		})
	}
}

func TestDockerStartRejectsUnverifiedAppImageBeforeDocker(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string) string
	}{
		{
			name: "missing",
			setup: func(_ *testing.T, _ string) string {
				return strings.Repeat("0", 64)
			},
		},
		{
			name: "digest mismatch",
			setup: func(t *testing.T, path string) string {
				writeAppImageFixture(t, path, "unexpected")
				return strings.Repeat("0", 64)
			},
		},
		{
			name: "not executable",
			setup: func(t *testing.T, path string) string {
				digest := writeAppImageFixture(t, path, "not-executable")
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatal(err)
				}
				return digest
			},
		},
		{
			name: "not ELF",
			setup: func(t *testing.T, path string) string {
				contents := []byte("not-an-ELF-appimage")
				if err := os.WriteFile(path, contents, 0o700); err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(contents)
				return hex.EncodeToString(digest[:])
			},
		},
		{
			name: "symlink",
			setup: func(t *testing.T, path string) string {
				target := path + ".target"
				digest := writeAppImageFixture(t, target, "symlink-target")
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
				return digest
			},
		},
		{
			name: "directory",
			setup: func(t *testing.T, path string) string {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				return strings.Repeat("0", 64)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			appImage := filepath.Join(root, "sensitive-client-name.AppImage")
			digest := test.setup(t, appImage)
			profile, err := (ProfileManager{}).Ensure(sharedAccountFixture(root))
			if err != nil {
				t.Fatal(err)
			}
			runner := &dockerFixtureRunner{image: fixtureRuntimeImage()}
			backend, err := NewDockerBackend(DockerConfig{
				Image: "wechatcopilot/wechat-runtime:test", AppImagePath: appImage,
				ExpectedAppImageSHA256: digest, Runner: runner,
			})
			if err != nil {
				t.Fatal(err)
			}
			err = backend.Start(context.Background(), profile)
			if err == nil {
				t.Fatal("Start accepted an unverified AppImage")
			}
			if len(runner.commands) != 0 {
				t.Fatalf("Docker was invoked before AppImage rejection: %#v", runner.commands)
			}
			if strings.Contains(err.Error(), appImage) || strings.Contains(err.Error(), digest) {
				t.Fatalf("verification error leaked artifact metadata: %v", err)
			}
		})
	}
}

func TestDockerStartReverifiesAppImageAfterInspect(t *testing.T) {
	root := t.TempDir()
	appImage := filepath.Join(root, "WeChat.AppImage")
	digest := writeAppImageFixture(t, appImage, "original")
	profile, err := (ProfileManager{}).Ensure(sharedAccountFixture(root))
	if err != nil {
		t.Fatal(err)
	}
	runner := &dockerFixtureRunner{
		image: fixtureRuntimeImage(),
		beforeContainerInspect: func() error {
			return os.WriteFile(appImage, append([]byte{0x7f, 'E', 'L', 'F'}, []byte("replacement")...), 0o700)
		},
	}
	backend, err := NewDockerBackend(DockerConfig{
		Image: "wechatcopilot/wechat-runtime:test", AppImagePath: appImage,
		ExpectedAppImageSHA256: digest, Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Start(context.Background(), profile); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Start error after AppImage replacement = %v", err)
	}
	for _, command := range runner.commands {
		if len(command.Args) > 0 && (command.Args[0] == "run" || command.Args[0] == "start") {
			t.Fatalf("Docker launched a replaced AppImage: %#v", command)
		}
	}
}

func TestDockerBackendClassifiesStaleSurfaceLocator(t *testing.T) {
	root := t.TempDir()
	appImage := filepath.Join(root, "WeChat.AppImage")
	digest := writeAppImageFixture(t, appImage, "fixture")
	profile, err := (ProfileManager{}).Ensure(sharedAccountFixture(root))
	if err != nil {
		t.Fatal(err)
	}
	runner := &dockerFixtureRunner{image: fixtureRuntimeImage()}
	backend, err := NewDockerBackend(DockerConfig{
		Image: "wechatcopilot/wechat-runtime:test", AppImagePath: appImage,
		ExpectedAppImageSHA256: digest, Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Start(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	runner.execResponse = []byte(`{"ok":false,"code":"ACTION_STALE","error":"surface changed"}`)
	if _, err := backend.ActSurface(context.Background(), "window-1", "opaque-locator", ""); !errors.Is(err, ErrActionStale) {
		t.Fatalf("ActSurface error = %v, want ErrActionStale", err)
	}
}

func TestDockerBackendUsesEmbeddedFrameForNamedSurface(t *testing.T) {
	root := t.TempDir()
	appImage := filepath.Join(root, "WeChat.AppImage")
	appDigest := writeAppImageFixture(t, appImage, "fixture")
	profile, err := (ProfileManager{}).Ensure(sharedAccountFixture(root))
	if err != nil {
		t.Fatal(err)
	}
	runner := &dockerFixtureRunner{image: fixtureRuntimeImage()}
	backend, err := NewDockerBackend(DockerConfig{
		Image: "wechatcopilot/wechat-runtime:test", AppImagePath: appImage,
		ExpectedAppImageSHA256: appDigest, Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Start(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	screenshot := []byte("one exact embedded frame")
	digest := sha256.Sum256(screenshot)
	runner.commands = nil
	runner.execResponse = mustMarshalFixture(map[string]any{
		"ok": true,
		"surface": map[string]any{
			"kind": "miniprogram", "title": "校园瞄", "generation": "generation-1",
			"window_identity": "window-token", "screenshot_base64": base64.StdEncoding.EncodeToString(screenshot),
			"screenshot_sha256": hex.EncodeToString(digest[:]),
			"viewport": map[string]any{
				"bounds": []int{0, 0, 80, 60}, "generation": "generation-1",
				"screenshot_sha256": hex.EncodeToString(digest[:]), "window_identity": "window-token",
			},
			"elements": []map[string]any{
				{
					"id": "element-1", "target_id": "target-1", "label": "宿舍", "role": "text",
					"bounds": []int{2, 3, 20, 10}, "source": "atspi", "confidence": 1.0,
					"locator": "element-locator-with-issued-time-100",
				},
				{
					"id": "element-2", "target_id": "target-2", "label": "宿舍", "role": "text",
					"bounds": []int{32, 3, 20, 10}, "source": "atspi", "confidence": 1.0,
					"locator": "element-locator-with-issued-time-100-other",
				},
			},
			"assets": []map[string]any{{
				"id": "asset-1", "token": "asset-backend", "role": "image", "bounds": []int{5, 7, 30, 20},
				"source": "atspi", "confidence": 0.95,
			}},
			"actions": []map[string]any{
				{
					"id": "action-1", "replay_id": "replay-1", "target_id": "target-1", "label": "宿舍", "kind": "activate",
					"risk": "medium", "effect": "navigate", "locator": "action-locator-with-issued-time-101",
				},
				{
					"id": "action-2", "replay_id": "replay-2", "target_id": "target-2", "label": "宿舍", "kind": "activate",
					"risk": "medium", "effect": "navigate", "locator": "action-locator-with-issued-time-101-other",
				},
			},
		},
	})
	surface, err := backend.OpenNamedSurface(context.Background(), "miniprogram", "校园瞄")
	if err != nil {
		t.Fatal(err)
	}
	if string(surface.Screenshot) != string(screenshot) || surface.ScreenshotSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("backend did not preserve the exact embedded screenshot")
	}
	if surface.WindowIdentity != "window-token" || surface.Viewport == nil || surface.Viewport.Width != 80 {
		t.Fatalf("surface frame identity/viewport = %#v", surface)
	}
	if len(surface.Elements) != 2 || surface.Elements[0].Bounds.X != 2 || surface.Elements[0].ActionID != "action-1" ||
		len(surface.Elements[0].ActionIDs) != 1 || surface.Elements[0].ActionIDs[0] != "action-1" ||
		surface.Elements[1].ActionID != "action-2" || surface.Elements[0].Label != surface.Elements[1].Label ||
		surface.Elements[0].TargetID == surface.Elements[1].TargetID {
		t.Fatalf("dynamic elements = %#v", surface.Elements)
	}
	if len(surface.Assets) != 1 || surface.Assets[0].Kind != "image" || len(surface.Actions) != 2 ||
		surface.Actions[0].Action.Effect != "navigate" || surface.Actions[0].Action.TargetID != "target-1" ||
		surface.Actions[0].ReplayID != "replay-1" || surface.Actions[1].Action.TargetID != "target-2" ||
		surface.Actions[1].ReplayID != "replay-2" {
		t.Fatalf("dynamic assets/actions = %#v / %#v", surface.Assets, surface.Actions)
	}
	if len(runner.commands) != 1 {
		t.Fatalf("embedded snapshot unexpectedly triggered another capture: %#v", runner.commands)
	}
	var request controlRequest
	if err := json.Unmarshal(runner.commands[0].Stdin, &request); err != nil {
		t.Fatal(err)
	}
	if request.Operation != "open_named_surface" || request.Kind != "miniprogram" || request.Name != "校园瞄" {
		t.Fatalf("named surface request = %#v", request)
	}
	runner.commands = nil
	if _, err := backend.SnapshotSurface(context.Background(), "window-token"); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(runner.commands[0].Stdin, &request); err != nil {
		t.Fatal(err)
	}
	if request.Operation != "snapshot_surface" || request.ExpectedWindowIdentity != "window-token" {
		t.Fatalf("bound snapshot request = %#v", request)
	}
}

func TestDockerBackendBindsMessageSurfaceKind(t *testing.T) {
	root := t.TempDir()
	appImage := filepath.Join(root, "WeChat.AppImage")
	appDigest := writeAppImageFixture(t, appImage, "fixture")
	profile, err := (ProfileManager{}).Ensure(sharedAccountFixture(root))
	if err != nil {
		t.Fatal(err)
	}
	runner := &dockerFixtureRunner{image: fixtureRuntimeImage()}
	backend, err := NewDockerBackend(DockerConfig{
		Image: "wechatcopilot/wechat-runtime:test", AppImagePath: appImage,
		ExpectedAppImageSHA256: appDigest, Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Start(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	runner.execResponse = mustMarshalFixture(map[string]any{
		"ok": true, "conversation_title": "Fixture chat", "conversation_locator": "conversation-locator",
		"messages": []map[string]any{{
			"text": "article", "kind": "link", "accessible_label": "Example article",
			"surface_kind": "web", "surface_locator": "signed-card-locator", "confidence": 0.9,
		}},
	})
	visible, err := backend.ReadVisibleMessages(context.Background(), "Fixture chat", "conversation-locator")
	if err != nil {
		t.Fatal(err)
	}
	if len(visible.Messages) != 1 || visible.Messages[0].SurfaceLocator != "signed-card-locator" {
		t.Fatalf("message surface locator was not parsed: %#v", visible.Messages)
	}
	screenshot := testSurfacePNG(t, 3, 2)
	digest := sha256.Sum256(screenshot)
	runner.execResponse = mustMarshalFixture(map[string]any{
		"ok": true,
		"surface": map[string]any{
			"kind": "web", "generation": "web-generation", "window_identity": "web-window",
			"screenshot_base64": base64.StdEncoding.EncodeToString(screenshot),
			"screenshot_sha256": hex.EncodeToString(digest[:]),
		},
	})
	runner.commands = nil
	_, err = backend.OpenSurface(context.Background(), SurfaceTarget{
		Reference: "surface-ref", ConversationID: "chat-1", ConversationTitle: "Fixture chat",
		ConversationLocator: "conversation-locator", AccessibleLabel: "Example article", Kind: "web",
		SurfaceLocator: "signed-card-locator",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 1 {
		t.Fatalf("message surface commands = %d, want 1", len(runner.commands))
	}
	var request controlRequest
	if err := json.Unmarshal(runner.commands[0].Stdin, &request); err != nil {
		t.Fatal(err)
	}
	if request.Operation != "open_surface" || request.Kind != "web" || request.SurfaceLocator != "signed-card-locator" {
		t.Fatalf("message surface request = %#v", request)
	}

	for _, kind := range []string{"", "article", "WEB"} {
		runner.commands = nil
		_, err := backend.OpenSurface(context.Background(), SurfaceTarget{Kind: kind})
		if !errors.Is(err, ErrClientIncompatible) {
			t.Fatalf("kind %q error = %v, want ErrClientIncompatible", kind, err)
		}
		if len(runner.commands) != 0 {
			t.Fatalf("invalid kind %q reached control backend: %#v", kind, runner.commands)
		}
	}
	runner.commands = nil
	if _, err := backend.OpenSurface(context.Background(), SurfaceTarget{Kind: "web"}); !errors.Is(err, ErrClientIncompatible) {
		t.Fatalf("missing message locator error = %v, want ErrClientIncompatible", err)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("missing message locator reached control backend: %#v", runner.commands)
	}

	runner.execResponse = mustMarshalFixture(map[string]any{
		"ok": true,
		"surface": map[string]any{
			"kind": "miniprogram", "generation": "mini-generation", "window_identity": "mini-window",
			"screenshot_base64": base64.StdEncoding.EncodeToString(screenshot),
			"screenshot_sha256": hex.EncodeToString(digest[:]),
		},
	})
	if _, err := backend.OpenSurface(context.Background(), SurfaceTarget{
		Kind: "web", SurfaceLocator: "signed-card-locator",
	}); !errors.Is(err, ErrClientIncompatible) {
		t.Fatalf("mismatched returned kind error = %v, want ErrClientIncompatible", err)
	}
}
