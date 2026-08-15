package wecom

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const maxAPKBytes int64 = 1 << 30

type ArtifactManager struct {
	Client *http.Client
}

// EnsureOfficialAPK verifies an existing APK or downloads it atomically from
// an allowlisted Tencent host. The caller must supply an independently
// obtained expected digest.
func (m ArtifactManager) EnsureOfficialAPK(ctx context.Context, rawURL, destination, expectedSHA256 string) error {
	if !sha256Pattern.MatchString(expectedSHA256) {
		return errors.New("expected APK SHA-256 must contain 64 hexadecimal characters")
	}
	expectedSHA256 = strings.ToLower(expectedSHA256)
	if digest, err := fileSHA256(destination); err == nil {
		if digest == expectedSHA256 {
			return validateAPKArchive(destination)
		}
		return fmt.Errorf("existing official APK digest mismatch: got %s", digest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect existing official APK: %w", err)
	}
	if rawURL == "" {
		return errors.New("official APK is missing and no download URL was configured")
	}
	client := m.Client
	if client == nil {
		client = &http.Client{}
	}
	originalRedirect := client.CheckRedirect
	clone := *client
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || !allowedAPKHost(req.URL.Hostname()) {
			return errors.New("APK redirect left approved Tencent hosts")
		}
		if len(via) >= 5 {
			return errors.New("too many APK redirects")
		}
		if originalRedirect != nil {
			return originalRedirect(req, via)
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("build APK download request: %w", err)
	}
	if req.URL.Scheme != "https" || !allowedAPKHost(req.URL.Hostname()) {
		return errors.New("official APK URL must use HTTPS on an approved Tencent host")
	}
	resp, err := clone.Do(req)
	if err != nil {
		return fmt.Errorf("download official APK: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download official APK: unexpected HTTP status %s", resp.Status)
	}
	if resp.ContentLength > maxAPKBytes {
		return fmt.Errorf("official APK exceeds %d bytes", maxAPKBytes)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create APK directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".wecom-apk-*")
	if err != nil {
		return fmt.Errorf("create APK temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("protect APK temporary file: %w", err)
	}
	hash := sha256.New()
	read := io.LimitReader(resp.Body, maxAPKBytes+1)
	written, copyErr := io.Copy(io.MultiWriter(tmp, hash), read)
	closeErr := tmp.Close()
	if copyErr != nil {
		return fmt.Errorf("write official APK: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close official APK: %w", closeErr)
	}
	if written > maxAPKBytes {
		return fmt.Errorf("official APK exceeds %d bytes", maxAPKBytes)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expectedSHA256 {
		return fmt.Errorf("downloaded official APK digest mismatch: got %s", actual)
	}
	if err := validateAPKArchive(tmpName); err != nil {
		return err
	}
	if err := os.Rename(tmpName, destination); err != nil {
		return fmt.Errorf("install verified official APK: %w", err)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validateAPKArchive(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return fmt.Errorf("read APK header: %w", err)
	}
	if string(magic) != "PK\x03\x04" {
		return errors.New("verified APK is not a ZIP/APK archive")
	}
	return nil
}

// snapshotAPK copies one no-follow file descriptor into the private account
// directory, binding validation and the bytes later handed to docker cp.
func snapshotAPK(source, privateDir, expectedSHA256 string) (path, digest string, err error) {
	if !filepath.IsAbs(source) || !filepath.IsAbs(privateDir) {
		return "", "", errors.New("APK source and private staging directory must be absolute")
	}
	info, err := os.Lstat(source)
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() {
		return "", "", errors.New("APK source must be a regular non-symlink file")
	}
	fd, err := syscall.Open(source, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", "", fmt.Errorf("open APK without following links: %w", err)
	}
	input := os.NewFile(uintptr(fd), source)
	defer input.Close()
	opened, err := input.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		return "", "", errors.New("opened APK is not a regular file")
	}
	if opened.Size() <= 0 || opened.Size() > maxAPKBytes {
		return "", "", fmt.Errorf("APK size must be between 1 and %d bytes", maxAPKBytes)
	}
	if err := os.MkdirAll(privateDir, 0o700); err != nil {
		return "", "", fmt.Errorf("create private APK staging directory: %w", err)
	}
	temporary, err := os.CreateTemp(privateDir, ".runtime-apk-*")
	if err != nil {
		return "", "", fmt.Errorf("create private APK snapshot: %w", err)
	}
	path = temporary.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if err = temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return "", "", err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(input, maxAPKBytes+1))
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		return "", "", errors.Join(copyErr, syncErr, closeErr)
	}
	if written != opened.Size() || written > maxAPKBytes {
		return "", "", errors.New("APK changed size while creating its private snapshot")
	}
	digest = hex.EncodeToString(hash.Sum(nil))
	if expectedSHA256 != "" && !strings.EqualFold(digest, expectedSHA256) {
		return "", "", fmt.Errorf("APK snapshot digest mismatch: got %s", digest)
	}
	if err = validateAPKArchive(path); err != nil {
		return "", "", err
	}
	return path, digest, nil
}
