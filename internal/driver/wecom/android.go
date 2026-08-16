package wecom

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maxCompanionWireBytes int64 = (8 << 20) + (64 << 10)
	resumedActivityProbe        = "/system/bin/dumpsys activity activities 2>/dev/null | /system/bin/toybox grep -E -m 1 'topResumedActivity=|mResumedActivity='"
)

// AndroidContainer invokes a small, fixed command set inside the exact
// ownership-checked Redroid container. It never opens an ADB host port.
type AndroidContainer struct {
	DockerBinary string
	Container    string
	Executor     Executor
	Verify       func(context.Context) error
}

func (a AndroidContainer) run(ctx context.Context, command string, args ...string) ([]byte, error) {
	if err := a.validate(); err != nil {
		return nil, err
	}
	if err := a.Verify(ctx); err != nil {
		return nil, fmt.Errorf("verify Redroid container before exec: %w", err)
	}
	execArgs := []string{"container", "exec", a.Container, command}
	execArgs = append(execArgs, args...)
	return a.Executor.Run(ctx, a.DockerBinary, execArgs...)
}

func (a AndroidContainer) runInput(ctx context.Context, input []byte, maxOutputBytes int64, command string, args ...string) ([]byte, error) {
	if err := a.validate(); err != nil {
		return nil, err
	}
	if err := a.Verify(ctx); err != nil {
		return nil, fmt.Errorf("verify Redroid container before interactive exec: %w", err)
	}
	execArgs := []string{"container", "exec", "--interactive", a.Container, command}
	execArgs = append(execArgs, args...)
	return a.Executor.RunInput(ctx, input, maxOutputBytes, a.DockerBinary, execArgs...)
}

func (a AndroidContainer) validate() error {
	if a.Executor == nil || a.Verify == nil {
		return errors.New("Android container transport is not configured")
	}
	if a.DockerBinary == "" || a.Container == "" {
		return errors.New("Android container command target is empty")
	}
	return nil
}

func (a AndroidContainer) WaitForBoot(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		out, err := a.run(probeCtx, "/system/bin/getprop", "sys.boot_completed")
		cancel()
		if err == nil && strings.TrimSpace(string(out)) == "1" {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for Redroid Android boot")
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

func (a AndroidContainer) ProbeNetcat(ctx context.Context) error {
	if _, err := a.run(ctx, "/system/bin/toybox", "nc", "--help"); err != nil {
		return fmt.Errorf("%w: Redroid image does not provide the required toybox nc applet: %w", ErrClientIncompatible, err)
	}
	return nil
}

func (a AndroidContainer) DisableNetworkADB(ctx context.Context) error {
	commands := [][]string{
		{"/system/bin/setprop", "persist.adb.tcp.port", "-1"},
		{"/system/bin/setprop", "service.adb.tcp.port", "-1"},
		{"/system/bin/stop", "adbd"},
	}
	for _, command := range commands {
		if _, err := a.run(ctx, command[0], command[1:]...); err != nil {
			return fmt.Errorf("%w: disable Redroid network ADB: %w", ErrClientIncompatible, err)
		}
	}
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		out, err := a.run(ctx, "/system/bin/toybox", "cat", table)
		if err != nil {
			return fmt.Errorf("%w: verify Redroid network listeners: %w", ErrClientIncompatible, err)
		}
		if procNetTCPHasListener(out, 5555) {
			return fmt.Errorf("%w: Redroid still exposes the unauthenticated ADB listener on TCP 5555", ErrClientIncompatible)
		}
	}
	return nil
}

func procNetTCPHasListener(contents []byte, port int) bool {
	expectedPort := strings.ToUpper(fmt.Sprintf("%04X", port))
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != "0A" {
			continue
		}
		address := fields[1]
		separator := strings.LastIndexByte(address, ':')
		if separator >= 0 && strings.EqualFold(address[separator+1:], expectedPort) {
			return true
		}
	}
	return false
}

func (a AndroidContainer) Install(ctx context.Context, hostAPKPath, containerAPKPath, expectedSHA256 string) (err error) {
	if !filepath.IsAbs(hostAPKPath) {
		return errors.New("staged APK path must be absolute")
	}
	if !validContainerAPKPath(containerAPKPath) {
		return errors.New("container APK path is outside the fixed staging directory")
	}
	if !sha256Pattern.MatchString(expectedSHA256) {
		return errors.New("staged APK digest is invalid")
	}
	expectedSHA256 = strings.ToLower(expectedSHA256)

	// A previous crash may have left only this exact digest-derived path.
	if _, err := a.run(ctx, "/system/bin/toybox", "rm", "-f", containerAPKPath); err != nil {
		return fmt.Errorf("clear stale container APK: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, cleanupErr := a.run(cleanupCtx, "/system/bin/toybox", "rm", "-f", containerAPKPath)
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove staged container APK: %w", cleanupErr))
		}
	}()

	if err := a.Verify(ctx); err != nil {
		return fmt.Errorf("verify Redroid container before APK copy: %w", err)
	}
	target := a.Container + ":" + containerAPKPath
	if _, err := a.Executor.Run(ctx, a.DockerBinary, "container", "cp", hostAPKPath, target); err != nil {
		return fmt.Errorf("copy verified APK into Redroid: %w", err)
	}
	digestOutput, err := a.run(ctx, "/system/bin/toybox", "sha256sum", containerAPKPath)
	if err != nil {
		return fmt.Errorf("hash copied container APK: %w", err)
	}
	fields := strings.Fields(string(digestOutput))
	if len(fields) < 1 || !strings.EqualFold(fields[0], expectedSHA256) {
		return errors.New("copied container APK digest mismatch")
	}
	installOutput, err := a.run(ctx, "/system/bin/pm", "install", "-r", containerAPKPath)
	if err != nil {
		return fmt.Errorf("install APK inside Redroid: %w", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(installOutput)), "Success") {
		return fmt.Errorf("%w: Android package manager did not confirm APK installation", ErrClientIncompatible)
	}
	return nil
}

func validContainerAPKPath(path string) bool {
	if !strings.HasPrefix(path, "/data/local/tmp/wechatcopilot-") || !strings.HasSuffix(path, ".apk") {
		return false
	}
	base := strings.TrimSuffix(strings.TrimPrefix(path, "/data/local/tmp/wechatcopilot-"), ".apk")
	if len(base) < 17 || len(base) > 96 {
		return false
	}
	for _, character := range base {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func (a AndroidContainer) PackageVersion(ctx context.Context, packageName string) (string, error) {
	out, err := a.run(ctx, "/system/bin/pm", "path", packageName)
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), "package:") {
		return "", fmt.Errorf("%w: Android package %q is not installed after verification", ErrClientIncompatible, packageName)
	}
	out, err = a.run(ctx, "/system/bin/dumpsys", "package", packageName)
	if err != nil {
		return "", fmt.Errorf("read Android package version: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "versionName=") {
			version := strings.TrimSpace(strings.TrimPrefix(line, "versionName="))
			if version != "" {
				return version, nil
			}
		}
	}
	return "unknown", nil
}

func (a AndroidContainer) ConfigureCompanion(ctx context.Context, packageName string) error {
	service := packageName + "/.WeComAccessibilityService"
	listener := packageName + "/.WeComNotificationListenerService"
	commands := [][]string{
		{"/system/bin/settings", "put", "secure", "enabled_accessibility_services", service},
		{"/system/bin/settings", "put", "secure", "accessibility_enabled", "1"},
		{"/system/bin/cmd", "notification", "allow_listener", listener},
		{"/system/bin/am", "broadcast", "-n", packageName + "/.BootstrapReceiver", "-a", packageName + ".BOOTSTRAP"},
	}
	for _, command := range commands {
		if _, err := a.run(ctx, command[0], command[1:]...); err != nil {
			return fmt.Errorf("configure companion command %q: %w", command[1:], err)
		}
	}
	return nil
}

func (a AndroidContainer) WaitForCompanionToken(ctx context.Context, packageName string, timeout time.Duration) (string, error) {
	tokenPath := "/data/user/0/" + packageName + "/files/rpc-token"
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		out, err := a.run(probeCtx, "/system/bin/toybox", "cat", tokenPath)
		cancel()
		if err == nil {
			token := strings.TrimSpace(string(out))
			if TokenValid(token) {
				return token, nil
			}
		}
		if time.Now().After(deadline) {
			return "", errors.New("timed out waiting for companion RPC token")
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}

func TokenValid(token string) bool {
	if len(token) < 43 || len(token) > 128 {
		return false
	}
	for _, character := range token {
		if (character < 'A' || character > 'Z') &&
			(character < 'a' || character > 'z') &&
			(character < '0' || character > '9') && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func (a AndroidContainer) LaunchWeCom(ctx context.Context, packageName string) error {
	const action = "android.intent.action.MAIN"
	const category = "android.intent.category.LAUNCHER"
	resolved, err := a.run(ctx, "/system/bin/cmd", "package", "resolve-activity", "--brief", "-a", action, "-c", category, packageName)
	if err != nil {
		return fmt.Errorf("resolve official WeCom launcher: %w", err)
	}
	component := ""
	for _, line := range strings.Split(string(resolved), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, packageName+"/") && !strings.ContainsAny(line, " \t\r") {
			component = line
		}
	}
	if component == "" {
		return fmt.Errorf("%w: Android package manager did not resolve the official WeCom launcher", ErrClientIncompatible)
	}
	output, err := a.run(ctx, "/system/bin/am", "start", "-W", "-a", action, "-c", category, "-n", component)
	if err != nil {
		return fmt.Errorf("launch official WeCom client: %w", err)
	}
	if !outputHasExactLine(output, "Status: ok") {
		return fmt.Errorf("%w: Android activity manager did not confirm the official WeCom launch", ErrClientIncompatible)
	}
	return nil
}

// ForegroundActivity returns only a strictly parsed component from Android's
// resumed-activity record. The fixed in-container filter prevents the full
// activity dump, which can contain intent details, from reaching the host.
func (a AndroidContainer) ForegroundActivity(ctx context.Context, packageName string) (string, error) {
	if packageName != DefaultWeComPackage {
		return "", errors.New("foreground activity probe is restricted to the official WeCom package")
	}
	output, err := a.run(ctx, "/system/bin/sh", "-c", resumedActivityProbe)
	if err != nil {
		return "", fmt.Errorf("inspect resumed official WeCom activity: %w", err)
	}
	activity, ok := parseResumedActivity(output, packageName)
	if !ok {
		return "", fmt.Errorf("%w: Android did not report the official WeCom activity in the foreground", ErrClientIncompatible)
	}
	return activity, nil
}

func parseResumedActivity(output []byte, packageName string) (string, bool) {
	prefix := packageName + "/"
	for _, field := range strings.Fields(string(output)) {
		component := strings.TrimRight(field, "}")
		if !strings.HasPrefix(component, prefix) {
			continue
		}
		className := strings.TrimPrefix(component, prefix)
		if strings.HasPrefix(className, ".") {
			className = packageName + className
		}
		if !strings.HasPrefix(className, packageName+".") || !validAndroidClassName(className) {
			return "", false
		}
		return className, true
	}
	return "", false
}

func validAndroidClassName(value string) bool {
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, character := range value {
		if (character < 'A' || character > 'Z') &&
			(character < 'a' || character > 'z') &&
			(character < '0' || character > '9') &&
			character != '.' && character != '_' && character != '$' {
			return false
		}
	}
	return true
}

func outputHasExactLine(output []byte, expected string) bool {
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == expected {
			return true
		}
	}
	return false
}

func (a AndroidContainer) Screenshot(ctx context.Context) ([]byte, error) {
	out, err := a.run(ctx, "/system/bin/screencap", "-p")
	if err != nil {
		return nil, fmt.Errorf("capture Redroid screen: %w", err)
	}
	if len(out) < 8 || string(out[:8]) != "\x89PNG\r\n\x1a\n" {
		return nil, errors.New("container screenshot did not return PNG data")
	}
	return out, nil
}

func (a AndroidContainer) CompanionRequest(ctx context.Context, devicePort int, rawRequest []byte) ([]byte, error) {
	if devicePort < 1024 || devicePort > 65535 {
		return nil, errors.New("invalid companion device port")
	}
	if len(rawRequest) == 0 || len(rawRequest) > 128<<10 {
		return nil, errors.New("companion wire request has invalid size")
	}
	return a.runInput(
		ctx,
		rawRequest,
		maxCompanionWireBytes,
		"/system/bin/toybox",
		"nc", "127.0.0.1", strconv.Itoa(devicePort),
	)
}
