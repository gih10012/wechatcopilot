package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

var (
	filesystemTypePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,31}$`)
	filesystemUUIDPattern = regexp.MustCompile(`^[A-Fa-f0-9]{8}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{4}-[A-Fa-f0-9]{12}$`)
)

type stateMountRequirement struct {
	Source string
	FSType string
	UUID   string
}

type mountInfoEntry struct {
	MountID    uint64
	Major      uint32
	Minor      uint32
	Root       string
	MountPoint string
	Options    string
	FSType     string
	Source     string
}

// RequiredStateMountGuard pins a validated state mount for the caller's
// lifetime so a normal unmount cannot expose the plaintext mountpoint below it.
type RequiredStateMountGuard struct {
	fd    int
	valid bool
	once  sync.Once
	err   error
}

func (g *RequiredStateMountGuard) Close() error {
	if g == nil || !g.valid {
		return nil
	}
	g.once.Do(func() { g.err = unix.Close(g.fd) })
	return g.err
}

// AcquireRequiredStateMount validates and pins the configured state mount. It
// returns nil when no mount requirement is configured.
func AcquireRequiredStateMount(path string) (*RequiredStateMountGuard, error) {
	requirement, required, err := stateMountRequirementFromEnv()
	if err != nil {
		return nil, err
	}
	if !required {
		persisted, err := HasPersistedStateMountGate()
		if err != nil {
			return nil, fmt.Errorf("inspect persisted state mount gate: %w", err)
		}
		if persisted {
			return nil, persistedStateMountGateError()
		}
		return nil, nil
	}
	return acquireStateMount(path, requirement)
}

// ValidateRequiredStateMount fails before any state directory is created when
// an operator pinned the state home to a dedicated mounted filesystem.
func ValidateRequiredStateMount(path string) error {
	guard, err := AcquireRequiredStateMount(path)
	if err != nil {
		return err
	}
	return guard.Close()
}

func requiredStateMountCheck(path string) (Check, bool, *RequiredStateMountGuard) {
	requirement, required, err := stateMountRequirementFromEnv()
	if !required {
		persisted, persistedErr := HasPersistedStateMountGate()
		if persistedErr != nil {
			err = fmt.Errorf("inspect persisted state mount gate: %w", persistedErr)
			required = true
		} else if persisted {
			err = persistedStateMountGateError()
			required = true
		} else {
			return Check{}, false, nil
		}
	}
	var guard *RequiredStateMountGuard
	if err == nil {
		guard, err = acquireStateMount(path, requirement)
	}
	if err != nil {
		return Check{
			Name:   "state_mount",
			OK:     false,
			Detail: err.Error(),
			Fix:    "Unlock and mount the exact configured state volume before starting the daemon.",
		}, true, nil
	}
	return Check{
		Name: "state_mount", OK: true,
		Detail: fmt.Sprintf("%s is %s from %s with filesystem UUID %s", path, requirement.FSType, requirement.Source, strings.ToLower(requirement.UUID)),
	}, true, guard
}

// HasPersistedStateMountGate reports whether a previous daemon installation
// requires state-mount constraints even when the current shell omitted them.
func HasPersistedStateMountGate() (bool, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return false, err
	}
	environmentPath := filepath.Join(configDir, "wechatcopilot", "state-mount.environment")
	if _, err := os.Lstat(environmentPath); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	unitPath := filepath.Join(configDir, "systemd", "user", "wechatcopilot.service")
	info, err := os.Lstat(unitPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("existing systemd unit %s is not a regular file", unitPath)
	}
	contents, err := os.ReadFile(unitPath)
	if err != nil {
		return false, err
	}
	escapedPath := strings.ReplaceAll(environmentPath, "%", "%%")
	requiredLine := "EnvironmentFile=" + strconv.Quote(escapedPath)
	for _, line := range strings.Split(string(contents), "\n") {
		if strings.TrimSpace(line) == requiredLine {
			return true, nil
		}
	}
	return false, nil
}

func persistedStateMountGateError() error {
	return errors.New("a persisted state mount gate exists; export WECHATCOPILOT_HOME and all three WECHATCOPILOT_STATE_MOUNT_* variables before running this command")
}

func stateMountRequirementFromEnv() (stateMountRequirement, bool, error) {
	requirement := stateMountRequirement{
		Source: strings.TrimSpace(os.Getenv(EnvStateMountSource)),
		FSType: strings.ToLower(strings.TrimSpace(os.Getenv(EnvStateMountFSType))),
		UUID:   strings.ToLower(strings.TrimSpace(os.Getenv(EnvStateMountUUID))),
	}
	required := requirement.Source != "" || requirement.FSType != "" || requirement.UUID != ""
	if !required {
		return stateMountRequirement{}, false, nil
	}
	if requirement.Source == "" || requirement.FSType == "" || requirement.UUID == "" {
		return requirement, true, fmt.Errorf("%s, %s, and %s must be set together", EnvStateMountSource, EnvStateMountFSType, EnvStateMountUUID)
	}
	if !filepath.IsAbs(requirement.Source) || filepath.Clean(requirement.Source) != requirement.Source {
		return requirement, true, fmt.Errorf("%s must be a clean absolute device path", EnvStateMountSource)
	}
	if !filesystemTypePattern.MatchString(requirement.FSType) {
		return requirement, true, fmt.Errorf("%s is invalid", EnvStateMountFSType)
	}
	if !filesystemUUIDPattern.MatchString(requirement.UUID) {
		return requirement, true, fmt.Errorf("%s must be a canonical filesystem UUID", EnvStateMountUUID)
	}
	return requirement, true, nil
}

// RequiredStateMountEnvironment returns normalized EnvironmentFile assignments
// for a configured state mount, or nil when the gate is disabled.
func RequiredStateMountEnvironment() ([]string, error) {
	requirement, required, err := stateMountRequirementFromEnv()
	if err != nil {
		return nil, err
	}
	if !required {
		return nil, nil
	}
	return []string{
		EnvStateMountSource + "=" + strconv.Quote(requirement.Source),
		EnvStateMountFSType + "=" + strconv.Quote(requirement.FSType),
		EnvStateMountUUID + "=" + strconv.Quote(requirement.UUID),
	}, nil
}

func acquireStateMount(path string, requirement stateMountRequirement) (_ *RequiredStateMountGuard, resultErr error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("state home must be absolute")
	}
	fd, stat, err := openDirectoryNoSymlinks(path, false)
	if err != nil {
		return nil, fmt.Errorf("open required state mount without creating it: %w", err)
	}
	guard := &RequiredStateMountGuard{fd: fd, valid: true}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, guard.Close())
		}
	}()
	if stat.Uid != uint32(os.Getuid()) || stat.Mode&0o7777 != 0o700 {
		return nil, fmt.Errorf("state mount must be owned by UID %d with mode 0700 and no special bits", os.Getuid())
	}

	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("open mount table: %w", err)
	}
	defer file.Close()
	var statx unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_STATX_SYNC_AS_STAT, unix.STATX_MNT_ID, &statx); err != nil {
		return nil, fmt.Errorf("resolve required state mount ID: %w", err)
	}
	if statx.Mask&unix.STATX_MNT_ID == 0 {
		return nil, errors.New("kernel did not report the required state mount ID")
	}
	entry, err := findMountInfo(file, filepath.Clean(path), statx.Mnt_id)
	if err != nil {
		return nil, err
	}
	if entry.Root != "/" {
		return nil, fmt.Errorf("state mount exposes filesystem root %s, expected /", entry.Root)
	}
	if entry.FSType != requirement.FSType {
		return nil, fmt.Errorf("state mount filesystem is %s, expected %s", entry.FSType, requirement.FSType)
	}
	if !mountOption(entry.Options, "rw") || !mountOption(entry.Options, "nosuid") || !mountOption(entry.Options, "nodev") {
		return nil, errors.New("state mount must be writable with nosuid and nodev options")
	}
	directoryMajor := unix.Major(uint64(stat.Dev))
	directoryMinor := unix.Minor(uint64(stat.Dev))
	if entry.Major != directoryMajor || entry.Minor != directoryMinor {
		return nil, fmt.Errorf("state mount table device %d:%d does not match the open directory device %d:%d", entry.Major, entry.Minor, directoryMajor, directoryMinor)
	}

	sourceMajor, sourceMinor, err := blockDeviceNumber(requirement.Source)
	if err != nil {
		return nil, fmt.Errorf("resolve configured state device: %w", err)
	}
	if directoryMajor != sourceMajor || directoryMinor != sourceMinor {
		return nil, fmt.Errorf("state mount uses device %d:%d, expected %s (%d:%d)", directoryMajor, directoryMinor, requirement.Source, sourceMajor, sourceMinor)
	}
	uuidPath := filepath.Join("/dev/disk/by-uuid", strings.ToLower(requirement.UUID))
	uuidMajor, uuidMinor, err := blockDeviceNumber(uuidPath)
	if err != nil {
		return nil, fmt.Errorf("resolve configured filesystem UUID: %w", err)
	}
	if directoryMajor != uuidMajor || directoryMinor != uuidMinor {
		return nil, fmt.Errorf("state mount filesystem UUID %s does not identify the mounted device", requirement.UUID)
	}
	return guard, nil
}

func findMountInfo(reader io.Reader, target string, requiredMountID ...uint64) (mountInfoEntry, error) {
	var found *mountInfoEntry
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			continue
		}
		separator := -1
		for index := 6; index < len(fields); index++ {
			if fields[index] == "-" {
				separator = index
				break
			}
		}
		if separator < 0 || separator+2 >= len(fields) {
			continue
		}
		mountPoint := decodeMountInfoField(fields[4])
		if mountPoint != target {
			continue
		}
		mountID, mountIDErr := strconv.ParseUint(fields[0], 10, 64)
		if mountIDErr != nil || len(requiredMountID) > 0 && mountID != requiredMountID[0] {
			continue
		}
		device := strings.SplitN(fields[2], ":", 2)
		if len(device) != 2 {
			continue
		}
		major, majorErr := strconv.ParseUint(device[0], 10, 32)
		minor, minorErr := strconv.ParseUint(device[1], 10, 32)
		if majorErr != nil || minorErr != nil {
			continue
		}
		entry := mountInfoEntry{
			MountID: mountID, Major: uint32(major), Minor: uint32(minor), Root: decodeMountInfoField(fields[3]), MountPoint: mountPoint,
			Options: fields[5], FSType: fields[separator+1], Source: decodeMountInfoField(fields[separator+2]),
		}
		found = &entry
	}
	if err := scanner.Err(); err != nil {
		return mountInfoEntry{}, fmt.Errorf("read mount table: %w", err)
	}
	if found == nil {
		return mountInfoEntry{}, fmt.Errorf("state home %s is not an exact mount point", target)
	}
	return *found, nil
}

func decodeMountInfoField(value string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(value)
}

func mountOption(options, expected string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == expected {
			return true
		}
	}
	return false
}

func blockDeviceNumber(path string) (uint32, uint32, error) {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return 0, 0, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFBLK {
		return 0, 0, fmt.Errorf("%s is not a block device", path)
	}
	return unix.Major(uint64(stat.Rdev)), unix.Minor(uint64(stat.Rdev)), nil
}
