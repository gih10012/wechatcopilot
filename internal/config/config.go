package config

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	EnvHome                      = "WECHATCOPILOT_HOME"
	EnvDocker                    = "WECHATCOPILOT_DOCKER"
	EnvLANAddress                = "WECHATCOPILOT_LAN_ADDRESS"
	EnvWeChatImage               = "WECHATCOPILOT_WECHAT_IMAGE"
	EnvWeChatAppImage            = "WECHATCOPILOT_WECHAT_APPIMAGE"
	EnvWeChatAppImageSHA256      = "WECHATCOPILOT_WECHAT_APPIMAGE_SHA256"
	EnvWeComRedroidImage         = "WECHATCOPILOT_WECOM_REDROID_IMAGE"
	EnvWeComAPKURL               = "WECHATCOPILOT_WECOM_APK_URL"
	EnvWeComAPKSHA256            = "WECHATCOPILOT_WECOM_APK_SHA256"
	EnvWeComAPK                  = "WECHATCOPILOT_WECOM_APK"
	EnvWeComCompanionAPK         = "WECHATCOPILOT_WECOM_COMPANION_APK"
	DefaultWeChatImage           = "wechatcopilot/wechat-runtime:v0.1.0"
	DefaultWeChatAppImageName    = "WeChat.AppImage"
	DefaultWeComAPKName          = "WeCom.apk"
	DefaultWeComCompanionAPKName = "wechatcopilot-companion.apk"
)

var (
	digestPattern      = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
	imagePattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,511}$`)
	pinnedImagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,511}@sha256:[a-f0-9]{64}$`)
)

type Paths struct {
	Home      string `json:"home"`
	Accounts  string `json:"accounts"`
	Downloads string `json:"downloads"`
	Runtime   string `json:"runtime"`
	Socket    string `json:"socket"`
	Registry  string `json:"registry"`
}

func ResolvePaths() (Paths, error) {
	home := os.Getenv(EnvHome)
	if home == "" {
		state := os.Getenv("XDG_STATE_HOME")
		if state == "" {
			userHome, err := os.UserHomeDir()
			if err != nil {
				return Paths{}, err
			}
			state = filepath.Join(userHome, ".local", "state")
		}
		home = filepath.Join(state, "wechatcopilot")
	}
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = filepath.Join(os.TempDir(), fmt.Sprintf("wechatcopilot-%d", os.Getuid()))
	} else {
		runtimeDir = filepath.Join(runtimeDir, "wechatcopilot")
	}
	if !filepath.IsAbs(home) {
		return Paths{}, fmt.Errorf("%s must be an absolute path", EnvHome)
	}
	return Paths{
		Home:      filepath.Clean(home),
		Accounts:  filepath.Join(home, "accounts"),
		Downloads: filepath.Join(home, "downloads"),
		Runtime:   runtimeDir,
		Socket:    filepath.Join(runtimeDir, "wechatcopilot.sock"),
		Registry:  filepath.Join(home, "accounts.json"),
	}, nil
}

func (p Paths) WithHome(home string) (Paths, error) {
	if home == "" {
		return p, nil
	}
	if !filepath.IsAbs(home) {
		return Paths{}, errors.New("state home must be an absolute path")
	}
	home = filepath.Clean(home)
	p.Home = home
	p.Accounts = filepath.Join(home, "accounts")
	p.Downloads = filepath.Join(home, "downloads")
	p.Registry = filepath.Join(home, "accounts.json")
	return p, nil
}

func (p Paths) Ensure() error {
	if runtime.GOOS != "linux" {
		return errors.New("wechatcopilot supports Linux only")
	}
	for _, dir := range []string{p.Home, p.Accounts, p.Downloads, p.Runtime} {
		if err := secureDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func secureDir(path string) error {
	fd, stat, err := openDirectoryNoSymlinks(path, true)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("state directory %s is owned by UID %d, expected %d", path, stat.Uid, os.Getuid())
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		return err
	}
	if err := unix.Fstat(fd, stat); err != nil {
		return err
	}
	if stat.Mode&0o777 != 0o700 {
		return fmt.Errorf("state directory %s has mode %04o, expected 0700", path, stat.Mode&0o777)
	}
	return nil
}

func openDirectoryNoSymlinks(path string, create bool) (int, *unix.Stat_t, error) {
	if !filepath.IsAbs(path) {
		return -1, nil, fmt.Errorf("state directory %q must be absolute", path)
	}
	clean := filepath.Clean(path)
	if clean == string(filepath.Separator) {
		return -1, nil, errors.New("state directory cannot be the filesystem root")
	}
	components := strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator))
	current, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, nil, err
	}
	closeCurrent := true
	defer func() {
		if closeCurrent {
			_ = unix.Close(current)
		}
	}()

	var currentStat unix.Stat_t
	if err := unix.Fstat(current, &currentStat); err != nil {
		return -1, nil, err
	}
	if !trustedPathComponent(&currentStat) {
		return -1, nil, errors.New("filesystem root has unsafe ownership or permissions")
	}
	for index, component := range components {
		created := false
		next, openErr := unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) && create {
			if err := unix.Mkdirat(current, component, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				return -1, nil, fmt.Errorf("create state path component %q: %w", component, err)
			}
			created = true
			next, openErr = unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if openErr != nil {
			return -1, nil, fmt.Errorf("open state path component %q without symlinks: %w", component, openErr)
		}
		if err := unix.Close(current); err != nil {
			_ = unix.Close(next)
			return -1, nil, err
		}
		current = next
		if err := unix.Fstat(current, &currentStat); err != nil {
			return -1, nil, err
		}
		if currentStat.Mode&unix.S_IFMT != unix.S_IFDIR {
			return -1, nil, fmt.Errorf("state path component %q is not a directory", component)
		}
		if created {
			if currentStat.Uid != uint32(os.Getuid()) {
				return -1, nil, fmt.Errorf("new state path component %q has unexpected owner UID %d", component, currentStat.Uid)
			}
			if err := unix.Fchmod(current, 0o700); err != nil {
				return -1, nil, err
			}
			if err := unix.Fstat(current, &currentStat); err != nil {
				return -1, nil, err
			}
		}
		if index < len(components)-1 && !trustedPathComponent(&currentStat) {
			return -1, nil, fmt.Errorf("state path component %q has unsafe ownership or permissions", component)
		}
	}
	closeCurrent = false
	stat := currentStat
	return current, &stat, nil
}

func trustedPathComponent(stat *unix.Stat_t) bool {
	if stat.Uid != 0 && stat.Uid != uint32(os.Getuid()) {
		return false
	}
	sharedWrite := stat.Mode & 0o022
	return sharedWrite == 0 || stat.Mode&unix.S_ISVTX != 0
}

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

func Doctor(paths Paths, runtimeChecks bool) []Check {
	checks := []Check{{Name: "platform", OK: runtime.GOOS == "linux", Detail: runtime.GOOS}}
	if err := paths.Ensure(); err != nil {
		checks = append(checks, Check{Name: "state_permissions", OK: false, Detail: err.Error()})
	} else {
		checks = append(checks, Check{Name: "state_permissions", OK: true, Detail: paths.Home})
		checks = append(checks, diskCheck(paths.Home))
		checks = append(checks, lockCheck(paths.Home))
	}
	checks = append(checks, privateDirectoryCheck("runtime_permissions", paths.Runtime, 0o700))
	checks = append(checks, daemonSocketCheck(paths.Socket))
	if !runtimeChecks {
		return checks
	}
	dockerBinary := envOrDefault(EnvDocker, "docker")
	checks = append(checks, commandCheck("docker", dockerBinary, "Install Docker and grant the current user access to its Unix socket."))
	checks = append(checks, dockerAccessCheck(dockerBinary))
	checks = append(checks, weChatAppImageCheck(
		envOrDefault(EnvWeChatAppImage, filepath.Join(paths.Downloads, DefaultWeChatAppImageName)),
		os.Getenv(EnvWeChatAppImageSHA256),
	))
	checks = append(checks, dockerImageCheck(
		"wechat_image", dockerBinary, envOrDefault(EnvWeChatImage, DefaultWeChatImage), false,
		"Build or load the configured WeChat runtime image locally; doctor never pulls it.",
	))
	checks = append(checks, binderCheck())
	checks = append(checks, dockerImageCheck(
		"wecom_redroid_image", dockerBinary, os.Getenv(EnvWeComRedroidImage), true,
		"Set WECHATCOPILOT_WECOM_REDROID_IMAGE to a locally available image pinned with @sha256:<64 lowercase hex>.",
	))
	officialAPK := envOrDefault(EnvWeComAPK, filepath.Join(paths.Downloads, DefaultWeComAPKName))
	companionAPK := envOrDefault(EnvWeComCompanionAPK, filepath.Join(paths.Downloads, DefaultWeComCompanionAPKName))
	checks = append(checks, apkCheck("wecom_apk", officialAPK, os.Getenv(EnvWeComAPKSHA256), false))
	checks = append(checks, apkCheck("wecom_companion_apk", companionAPK, "", true))
	checks = append(checks, distinctArtifactsCheck(officialAPK, companionAPK))
	return checks
}

func commandCheck(name, binary, fix string) Check {
	path, err := exec.LookPath(binary)
	if err != nil {
		return Check{Name: name, OK: false, Detail: fmt.Sprintf("%s not found", binary), Fix: fix}
	}
	return Check{Name: name, OK: true, Detail: path}
}

func dockerAccessCheck(binary string) Check {
	cmd := exec.Command(binary, "info", "--format", "{{.ServerVersion}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return Check{Name: "docker_access", OK: false, Detail: detail, Fix: "Grant this user Docker socket access, then start a new login session."}
	}
	return Check{Name: "docker_access", OK: true, Detail: strings.TrimSpace(string(out))}
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func fileUID(info fs.FileInfo) (uint32, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("filesystem did not expose Unix ownership")
	}
	return stat.Uid, nil
}

func privateDirectoryCheck(name, path string, expected fs.FileMode) Check {
	fd, stat, err := openDirectoryNoSymlinks(path, false)
	if err != nil {
		return Check{Name: name, OK: false, Detail: err.Error()}
	}
	defer unix.Close(fd)
	if stat.Uid != uint32(os.Getuid()) {
		return Check{Name: name, OK: false, Detail: fmt.Sprintf("owner UID %d, expected %d", stat.Uid, os.Getuid())}
	}
	mode := fs.FileMode(stat.Mode & 0o777)
	if mode != expected.Perm() {
		return Check{Name: name, OK: false, Detail: fmt.Sprintf("mode %04o, expected %04o", mode, expected.Perm())}
	}
	return Check{Name: name, OK: true, Detail: fmt.Sprintf("%s (UID %d, mode %04o)", path, stat.Uid, mode)}
}

func daemonSocketCheck(path string) Check {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Check{Name: "daemon_socket", OK: true, Detail: "daemon stopped (socket absent)"}
	}
	if err != nil {
		return Check{Name: "daemon_socket", OK: false, Detail: err.Error()}
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return Check{Name: "daemon_socket", OK: false, Detail: "socket path must be a non-symlink Unix socket"}
	}
	uid, err := fileUID(info)
	if err != nil {
		return Check{Name: "daemon_socket", OK: false, Detail: err.Error()}
	}
	if uid != uint32(os.Getuid()) {
		return Check{Name: "daemon_socket", OK: false, Detail: fmt.Sprintf("owner UID %d, expected %d", uid, os.Getuid())}
	}
	if info.Mode().Perm() != 0o600 {
		return Check{Name: "daemon_socket", OK: false, Detail: fmt.Sprintf("mode %04o, expected 0600", info.Mode().Perm())}
	}
	return Check{Name: "daemon_socket", OK: true, Detail: fmt.Sprintf("%s (UID %d, mode 0600)", path, uid)}
}

func weChatAppImageCheck(path, expectedDigest string) Check {
	err := validateWeChatAppImage(path, expectedDigest)
	if err != nil {
		return Check{
			Name: "wechat_appimage", OK: false, Detail: err.Error(),
			Fix: "Set WECHATCOPILOT_WECHAT_APPIMAGE and WECHATCOPILOT_WECHAT_APPIMAGE_SHA256 to a verified official executable AppImage.",
		}
	}
	return Check{Name: "wechat_appimage", OK: true, Detail: path + " (ELF and SHA-256 verified)"}
}

func validateWeChatAppImage(path, expectedDigest string) error {
	if !digestPattern.MatchString(expectedDigest) {
		return errors.New("WECHATCOPILOT_WECHAT_APPIMAGE_SHA256 must contain exactly 64 hexadecimal characters")
	}
	info, err := regularArtifact(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o111 == 0 {
		return errors.New("official WeChat AppImage is not executable")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	var magic [4]byte
	_, readErr := io.ReadFull(file, magic[:])
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if magic != [4]byte{0x7f, 'E', 'L', 'F'} {
		return errors.New("official WeChat AppImage does not have an ELF header")
	}
	digest, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(digest, expectedDigest) {
		return fmt.Errorf("official WeChat AppImage SHA-256 mismatch: got %s", digest)
	}
	return nil
}

func dockerImageCheck(name, binary, image string, requireDigest bool, fix string) Check {
	pattern := imagePattern
	if requireDigest {
		pattern = pinnedImagePattern
	}
	if !pattern.MatchString(image) {
		detail := "invalid local Docker image reference"
		if requireDigest {
			detail = "image reference must end with @sha256:<64 lowercase hex>"
		}
		return Check{Name: name, OK: false, Detail: detail, Fix: fix}
	}
	cmd := exec.Command(binary, "image", "inspect", "--format", "{{.Id}}", image)
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return Check{Name: name, OK: false, Detail: detail, Fix: fix}
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return Check{Name: name, OK: false, Detail: "Docker returned an empty local image ID", Fix: fix}
	}
	return Check{Name: name, OK: true, Detail: image + " -> " + id}
}

func apkCheck(name, path, expectedDigest string, requireClasses bool) Check {
	err := validateAPK(path, expectedDigest, requireClasses)
	if err != nil {
		fix := "Provide the verified official WeCom APK and its SHA-256."
		if requireClasses {
			fix = "Build or install the signed wechatcopilot companion APK at the configured absolute path."
		}
		return Check{Name: name, OK: false, Detail: err.Error(), Fix: fix}
	}
	detail := path + " (APK structure verified)"
	if expectedDigest != "" {
		detail = path + " (APK structure and SHA-256 verified)"
	}
	return Check{Name: name, OK: true, Detail: detail}
}

func validateAPK(path, expectedDigest string, requireClasses bool) error {
	if !requireClasses && !digestPattern.MatchString(expectedDigest) {
		return errors.New("official WeCom APK SHA-256 must contain exactly 64 hexadecimal characters")
	}
	if _, err := regularArtifact(path); err != nil {
		return err
	}
	archive, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("open APK ZIP: %w", err)
	}
	defer archive.Close()
	hasManifest := false
	hasClasses := false
	for _, file := range archive.File {
		switch file.Name {
		case "AndroidManifest.xml":
			hasManifest = true
		case "classes.dex":
			hasClasses = true
		}
	}
	if !hasManifest {
		return errors.New("APK does not contain AndroidManifest.xml")
	}
	if requireClasses && !hasClasses {
		return errors.New("companion APK does not contain classes.dex")
	}
	if expectedDigest != "" {
		digest, err := fileSHA256(path)
		if err != nil {
			return err
		}
		if !strings.EqualFold(digest, expectedDigest) {
			return fmt.Errorf("official WeCom APK SHA-256 mismatch: got %s", digest)
		}
	}
	return nil
}

func regularArtifact(path string) (fs.FileInfo, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("artifact path %q must be absolute", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("artifact %s must be a regular, non-symlink file", path)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("artifact %s is empty", path)
	}
	return info, nil
}

func distinctArtifactsCheck(official, companion string) Check {
	if !filepath.IsAbs(official) || !filepath.IsAbs(companion) {
		return Check{Name: "wecom_apk_separation", OK: false, Detail: "both APK paths must be absolute"}
	}
	officialInfo, officialErr := os.Stat(official)
	companionInfo, companionErr := os.Stat(companion)
	if officialErr != nil || companionErr != nil {
		return Check{Name: "wecom_apk_separation", OK: false, Detail: errors.Join(officialErr, companionErr).Error()}
	}
	officialResolved, officialErr := filepath.EvalSymlinks(official)
	companionResolved, companionErr := filepath.EvalSymlinks(companion)
	if officialErr != nil || companionErr != nil {
		return Check{Name: "wecom_apk_separation", OK: false, Detail: errors.Join(officialErr, companionErr).Error()}
	}
	if os.SameFile(officialInfo, companionInfo) || filepath.Clean(officialResolved) == filepath.Clean(companionResolved) {
		return Check{Name: "wecom_apk_separation", OK: false, Detail: "official and companion APK paths resolve to the same file"}
	}
	return Check{Name: "wecom_apk_separation", OK: true, Detail: "official and companion APKs are distinct files"}
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func binderCheck() Check {
	contents, err := os.ReadFile("/proc/filesystems")
	if err != nil || !containsLine(contents, "binder") {
		return Check{Name: "binderfs", OK: false, Detail: "binder filesystem unavailable", Fix: "Enable Android Binder/BinderFS in the host kernel."}
	}
	return Check{Name: "binderfs", OK: true, Detail: "kernel exposes binder filesystem"}
}

func containsLine(contents []byte, needle string) bool {
	for _, line := range splitLines(string(contents)) {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[len(fields)-1] == needle {
			return true
		}
	}
	return false
}

func splitLines(value string) []string {
	var lines []string
	start := 0
	for i, r := range value {
		if r == '\n' {
			lines = append(lines, value[start:i])
			start = i + 1
		}
	}
	if start < len(value) {
		lines = append(lines, value[start:])
	}
	return lines
}

func diskCheck(path string) Check {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return Check{Name: "state_space", OK: false, Detail: err.Error()}
	}
	available := uint64(stat.Bavail) * uint64(stat.Bsize)
	ok := available >= 30*1024*1024*1024
	check := Check{Name: "state_space", OK: ok, Detail: fmt.Sprintf("%.1f GiB available", float64(available)/(1024*1024*1024))}
	if !ok {
		check.Fix = "Set WECHATCOPILOT_HOME to a Linux filesystem with at least 30 GiB free before real login."
	}
	return check
}

func lockCheck(path string) Check {
	file, err := os.OpenFile(filepath.Join(path, ".lock-check"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return Check{Name: "state_locks", OK: false, Detail: err.Error()}
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return Check{Name: "state_locks", OK: false, Detail: err.Error()}
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return Check{Name: "state_locks", OK: true, Detail: "advisory locks work"}
}

func AtomicWrite(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
