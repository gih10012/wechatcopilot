package wechat

import (
	"context"
	"crypto/sha256"
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
	if _, err := backend.ActSurface(context.Background(), "opaque-locator", ""); !errors.Is(err, ErrActionStale) {
		t.Fatalf("ActSurface error = %v, want ErrActionStale", err)
	}
}
