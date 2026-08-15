package wechat

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	shared "github.com/gih10012/wechatcopilot/internal/driver"
)

var accountIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

const profileSchemaVersion = 1

// Profile contains only paths owned by wechatcopilot. ClientHome is mounted as
// HOME, so the official client never observes or mutates the operator's HOME.
type Profile struct {
	AccountID  string
	Alias      string
	Root       string
	ClientHome string
	Files      string
	Runtime    string
	MachineID  string
	Hostname   string
}

type profileMetadata struct {
	SchemaVersion int       `json:"schema_version"`
	AccountID     string    `json:"account_id"`
	Alias         string    `json:"alias"`
	MachineID     string    `json:"machine_id"`
	Hostname      string    `json:"hostname"`
	CreatedAt     time.Time `json:"created_at"`
}

// ProfileManager validates and creates isolated, persistent client profiles.
type ProfileManager struct {
	// ProtectedPaths is primarily useful to embed additional operator-owned
	// client paths. The current user's ~/.xwechat is always protected.
	ProtectedPaths []string
	Now            func() time.Time
}

func (m ProfileManager) Ensure(account shared.AccountRuntime) (Profile, error) {
	if !accountIDPattern.MatchString(account.AccountID) {
		return Profile{}, fmt.Errorf("invalid account id %q", account.AccountID)
	}
	if strings.TrimSpace(account.Alias) == "" {
		return Profile{}, errors.New("account alias is required")
	}
	if !filepath.IsAbs(account.StateDir) || !filepath.IsAbs(account.RuntimeDir) {
		return Profile{}, errors.New("account state_dir and runtime_dir must be absolute")
	}

	stateRoot, err := cleanAbsolute(account.StateDir)
	if err != nil {
		return Profile{}, fmt.Errorf("resolve state directory: %w", err)
	}
	runtimeRoot, err := cleanAbsolute(account.RuntimeDir)
	if err != nil {
		return Profile{}, fmt.Errorf("resolve runtime directory: %w", err)
	}
	if err := m.rejectProtected(stateRoot); err != nil {
		return Profile{}, err
	}
	if err := m.rejectProtected(runtimeRoot); err != nil {
		return Profile{}, err
	}

	profile := Profile{
		AccountID:  account.AccountID,
		Alias:      account.Alias,
		Root:       stateRoot,
		ClientHome: filepath.Join(stateRoot, "client-home"),
		Files:      filepath.Join(stateRoot, "client-files"),
		Runtime:    runtimeRoot,
		Hostname:   hostnameFor(account.AccountID),
	}
	for _, path := range []string{profile.Root, profile.ClientHome, profile.Files, profile.Runtime} {
		if err := ensurePrivateDirectory(path); err != nil {
			return Profile{}, err
		}
	}

	metadataPath := filepath.Join(profile.Root, "profile.json")
	metadata, err := readProfileMetadata(metadataPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Profile{}, err
	}
	if errors.Is(err, fs.ErrNotExist) {
		machineID, err := randomMachineID()
		if err != nil {
			return Profile{}, err
		}
		now := time.Now().UTC()
		if m.Now != nil {
			now = m.Now().UTC()
		}
		metadata = profileMetadata{
			SchemaVersion: profileSchemaVersion,
			AccountID:     account.AccountID,
			Alias:         account.Alias,
			MachineID:     machineID,
			Hostname:      profile.Hostname,
			CreatedAt:     now,
		}
		if err := writeJSONAtomic(metadataPath, metadata, 0o600); err != nil {
			return Profile{}, err
		}
	}
	if metadata.SchemaVersion != profileSchemaVersion || metadata.AccountID != account.AccountID {
		return Profile{}, fmt.Errorf("profile metadata does not belong to account %q", account.AccountID)
	}
	profile.MachineID = metadata.MachineID
	profile.Hostname = metadata.Hostname

	machineIDPath := filepath.Join(profile.Root, "machine-id")
	expectedMachineID := []byte(profile.MachineID + "\n")
	if err := writeIfMissing(machineIDPath, expectedMachineID, 0o600); err != nil {
		return Profile{}, err
	}
	storedMachineID, err := readPrivateRegularFile(machineIDPath)
	if err != nil {
		return Profile{}, err
	}
	if string(storedMachineID) != string(expectedMachineID) {
		return Profile{}, errors.New("profile machine-id does not match profile metadata")
	}
	return profile, nil
}

func (m ProfileManager) rejectProtected(target string) error {
	protected := append([]string(nil), m.ProtectedPaths...)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		protected = append(protected, filepath.Join(home, ".xwechat"))
	}
	for _, raw := range protected {
		path, err := cleanAbsolute(raw)
		if err != nil {
			continue
		}
		if pathWithin(target, path) || pathWithin(path, target) {
			return fmt.Errorf("refusing to use protected client path %q", path)
		}
	}
	return nil
}

func cleanAbsolute(path string) (string, error) {
	if path == "" {
		return "", errors.New("empty path")
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	// Resolve the nearest existing parent so a symlink cannot redirect a newly
	// created profile into an operator-owned client directory.
	current := abs
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return abs, nil
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func pathWithin(candidate, parent string) bool {
	rel, err := filepath.Rel(parent, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func ensurePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create private directory %q: %w", path, err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("profile path %q must be a real directory", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("secure directory %q: %w", path, err)
		}
	}
	return nil
}

func randomMachineID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate stable machine id: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func hostnameFor(accountID string) string {
	sum := sha256.Sum256([]byte(accountID))
	return "wx-" + hex.EncodeToString(sum[:6])
}

func readProfileMetadata(path string) (profileMetadata, error) {
	data, err := readPrivateRegularFile(path)
	if err != nil {
		return profileMetadata{}, err
	}
	var metadata profileMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return profileMetadata{}, fmt.Errorf("decode profile metadata: %w", err)
	}
	return metadata, nil
}

func readPrivateRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("private state path %q must be a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("private state path %q is accessible by other users", path)
	}
	return os.ReadFile(path)
}

func writeJSONAtomic(path string, value any, mode fs.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeAtomic(path, data, mode)
}

func writeIfMissing(path string, data []byte, mode fs.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".wechatcopilot-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
