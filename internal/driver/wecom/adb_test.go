package wecom

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAndroidContainerPackageVersionRequiresInstalledPackage(t *testing.T) {
	executor := &sequenceExecutor{results: []executorResult{
		{output: []byte("package:/data/app/com.tencent.wework/base.apk\n")},
		{output: []byte("Packages:\n  versionCode=123\n  versionName=5.0.0\n")},
	}}
	android := AndroidContainer{
		DockerBinary: "docker", Container: "synthetic", Executor: executor,
		Verify: func(context.Context) error { return nil },
	}
	version, err := android.PackageVersion(context.Background(), DefaultWeComPackage)
	if err != nil {
		t.Fatal(err)
	}
	if version != "5.0.0" {
		t.Fatalf("unexpected package version %q", version)
	}
}

type secretCheckingExecutor struct {
	t     *testing.T
	token string
	seen  bool
}

func (e *secretCheckingExecutor) Run(context.Context, string, ...string) ([]byte, error) {
	return nil, nil
}

func (e *secretCheckingExecutor) RunInput(_ context.Context, input []byte, maxOutput int64, name string, args ...string) ([]byte, error) {
	e.t.Helper()
	e.seen = true
	if maxOutput != maxCompanionWireBytes {
		e.t.Fatalf("unexpected response bound %d", maxOutput)
	}
	joinedArgs := name + " " + strings.Join(args, " ")
	if strings.Contains(joinedArgs, e.token) {
		e.t.Fatal("companion token leaked into process arguments")
	}
	if !bytes.Contains(input, []byte("Authorization: Bearer "+e.token+"\r\n")) {
		e.t.Fatal("companion token was not carried in private stdin")
	}
	if !strings.Contains(joinedArgs, "container exec --interactive synthetic /system/bin/toybox nc 127.0.0.1 18765") {
		e.t.Fatalf("unexpected companion command: %s", joinedArgs)
	}
	body := `{"ok":true}`
	return []byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)), nil
}

func TestContainerCompanionCarriesTokenOnlyInStdin(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	executor := &secretCheckingExecutor{t: t, token: token}
	android := AndroidContainer{
		DockerBinary: "docker", Container: "synthetic", Executor: executor,
		Verify: func(context.Context) error { return nil },
	}
	client, err := newContainerCompanionClient(android, DefaultCompanionPort, token, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !executor.seen {
		t.Fatal("interactive container transport was not used")
	}
}

func TestAndroidInstallCopiesDigestCheckedSnapshotAndCleansIt(t *testing.T) {
	contents := []byte("PK\x03\x04synthetic")
	hostPath := filepath.Join(t.TempDir(), "snapshot.apk")
	if err := os.WriteFile(hostPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := fileSHA256(hostPath)
	if err != nil {
		t.Fatal(err)
	}
	containerPath := containerAPKPath("companion", digest)
	var commands [][]string
	executor := functionExecutor(func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, append([]string{name}, args...))
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "sha256sum"):
			return []byte(digest + "  " + containerPath + "\n"), nil
		case strings.Contains(joined, "/system/bin/pm install"):
			return []byte("Success\n"), nil
		default:
			return nil, nil
		}
	})
	android := AndroidContainer{
		DockerBinary: "docker", Container: "synthetic", Executor: executor,
		Verify: func(context.Context) error { return nil },
	}
	if err := android.Install(context.Background(), hostPath, containerPath, digest); err != nil {
		t.Fatal(err)
	}
	joined := fmt.Sprint(commands)
	if !strings.Contains(joined, "container cp "+hostPath+" synthetic:"+containerPath) ||
		!strings.Contains(joined, "/system/bin/pm install -r "+containerPath) {
		t.Fatalf("install did not use the fixed copy/install path: %s", joined)
	}
	if strings.Count(joined, "/system/bin/toybox rm -f "+containerPath) != 2 {
		t.Fatalf("staged APK was not cleaned before and after install: %s", joined)
	}
	if strings.Contains(joined, " sh ") || strings.Contains(joined, "adb") {
		t.Fatalf("install escaped the fixed direct-exec command set: %s", joined)
	}
}

func TestProcNetTCPListenerDetection(t *testing.T) {
	listening := []byte("sl local_address rem_address st\n 0: 00000000:15B3 00000000:0000 0A\n")
	if !procNetTCPHasListener(listening, 5555) {
		t.Fatal("failed to detect a listening ADB socket")
	}
	if procNetTCPHasListener([]byte("0: 0100007F:15B3 00000000:0000 01\n"), 5555) {
		t.Fatal("non-listening socket was treated as an ADB listener")
	}
}

var _ http.RoundTripper = (*containerRoundTripper)(nil)

func TestAndroidContainerScreenshotRejectsNonPNG(t *testing.T) {
	executor := &sequenceExecutor{results: []executorResult{{output: []byte("not a PNG")}}}
	android := AndroidContainer{
		DockerBinary: "docker", Container: "synthetic", Executor: executor,
		Verify: func(context.Context) error { return nil },
	}
	if _, err := android.Screenshot(context.Background()); err == nil {
		t.Fatal("expected invalid screenshot to be rejected")
	}
}

func TestAndroidContainerLaunchWeComResolvesAndConfirmsLauncher(t *testing.T) {
	var commands []string
	executor := functionExecutor(func(_ context.Context, name string, args ...string) ([]byte, error) {
		command := strings.Join(append([]string{name}, args...), " ")
		commands = append(commands, command)
		if strings.Contains(command, " resolve-activity ") {
			return []byte("priority=0\n" + DefaultWeComPackage + "/.launch.LaunchSplashActivity\n"), nil
		}
		return []byte("Starting: Intent {...}\nStatus: ok\nComplete\n"), nil
	})
	android := AndroidContainer{
		DockerBinary: "docker", Container: "synthetic", Executor: executor,
		Verify: func(context.Context) error { return nil },
	}
	if err := android.LaunchWeCom(context.Background(), DefaultWeComPackage); err != nil {
		t.Fatal(err)
	}
	expected := []string{
		"docker container exec synthetic /system/bin/cmd package resolve-activity --brief -a android.intent.action.MAIN -c android.intent.category.LAUNCHER " + DefaultWeComPackage,
		"docker container exec synthetic /system/bin/am start -W -a android.intent.action.MAIN -c android.intent.category.LAUNCHER -n " + DefaultWeComPackage + "/.launch.LaunchSplashActivity",
	}
	if fmt.Sprint(commands) != fmt.Sprint(expected) {
		t.Fatalf("WeCom launch commands = %q, want %q", commands, expected)
	}
}

func TestAndroidContainerLaunchWeComRejectsUnconfirmedLauncher(t *testing.T) {
	executor := &sequenceExecutor{results: []executorResult{
		{output: []byte(DefaultWeComPackage + "/.launch.LaunchSplashActivity\n")},
		{output: []byte("Error: Activity not started\n")},
	}}
	android := AndroidContainer{
		DockerBinary: "docker", Container: "synthetic", Executor: executor,
		Verify: func(context.Context) error { return nil },
	}
	if err := android.LaunchWeCom(context.Background(), DefaultWeComPackage); !errors.Is(err, ErrClientIncompatible) {
		t.Fatalf("unconfirmed launcher error = %v, want ErrClientIncompatible", err)
	}
}

func TestAndroidContainerForegroundActivityReturnsOnlyOfficialComponent(t *testing.T) {
	var command string
	executor := functionExecutor(func(_ context.Context, name string, args ...string) ([]byte, error) {
		command = strings.Join(append([]string{name}, args...), " ")
		return []byte("topResumedActivity=ActivityRecord{efce792 u0 " + DefaultWeComPackage + "/.login.controller.LoginWxAuthActivity} t9}\n"), nil
	})
	android := AndroidContainer{
		DockerBinary: "docker", Container: "synthetic", Executor: executor,
		Verify: func(context.Context) error { return nil },
	}
	activity, err := android.ForegroundActivity(context.Background(), DefaultWeComPackage)
	if err != nil {
		t.Fatal(err)
	}
	if activity != "com.tencent.wework.login.controller.LoginWxAuthActivity" {
		t.Fatalf("foreground activity = %q", activity)
	}
	expected := "docker container exec synthetic /system/bin/sh -c " + resumedActivityProbe
	if command != expected {
		t.Fatalf("foreground command = %q, want %q", command, expected)
	}
}

func TestParseResumedActivityRejectsForeignAndMalformedComponents(t *testing.T) {
	tests := []string{
		"topResumedActivity=ActivityRecord{x u0 com.android.settings/.Settings}",
		"topResumedActivity=ActivityRecord{x u0 " + DefaultWeComPackage + "/other.package.Activity}",
		"topResumedActivity=ActivityRecord{x u0 " + DefaultWeComPackage + "/.login.Bad-Activity}",
	}
	for _, output := range tests {
		if activity, ok := parseResumedActivity([]byte(output), DefaultWeComPackage); ok {
			t.Fatalf("parseResumedActivity(%q) = %q, want rejection", output, activity)
		}
	}
}
