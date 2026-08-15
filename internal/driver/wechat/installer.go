package wechat

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultMaximumClientBytes int64 = 2 << 30

var defaultOfficialHosts = map[string]struct{}{
	"linux.weixin.qq.com": {},
	"dldir1.qq.com":       {},
	"dldir1v6.qq.com":     {},
	"weixin.qq.com":       {},
	"res.wx.qq.com":       {},
}

type InstallRequest struct {
	URL              string
	SHA256           string
	Destination      string
	AllowedHosts     []string
	MaximumBytes     int64
	AllowHTTPForTest bool
}

type InstallResult struct {
	Path      string
	SHA256    string
	Size      int64
	SourceURL string
}

// ClientInstaller performs an explicit, checksum-pinned download from an
// allow-listed official host. Nothing invokes it during normal Driver startup.
type ClientInstaller struct {
	HTTPClient *http.Client
}

func (i ClientInstaller) Download(ctx context.Context, request InstallRequest) (InstallResult, error) {
	expected, err := parseSHA256(request.SHA256)
	if err != nil {
		return InstallResult{}, err
	}
	destination, err := cleanAbsolute(request.Destination)
	if err != nil {
		return InstallResult{}, fmt.Errorf("resolve client destination: %w", err)
	}
	if filepath.Ext(destination) != ".AppImage" {
		return InstallResult{}, errors.New("official Linux client destination must end in .AppImage")
	}
	if err := (ProfileManager{}).rejectProtected(destination); err != nil {
		return InstallResult{}, err
	}
	allowed := allowedHostSet(request.AllowedHosts)
	source, err := validateDownloadURL(request.URL, allowed, request.AllowHTTPForTest)
	if err != nil {
		return InstallResult{}, err
	}
	if err := ensurePrivateDirectory(filepath.Dir(destination)); err != nil {
		return InstallResult{}, err
	}

	if result, ok, err := matchingExistingClient(destination, expected); err != nil {
		return InstallResult{}, err
	} else if ok {
		result.SourceURL = source.String()
		return result, nil
	}

	client := i.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	clone := *client
	previousRedirect := clone.CheckRedirect
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many client download redirects")
		}
		if _, err := validateDownloadURL(req.URL.String(), allowed, request.AllowHTTPForTest); err != nil {
			return err
		}
		if previousRedirect != nil {
			return previousRedirect(req, via)
		}
		return nil
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, source.String(), nil)
	if err != nil {
		return InstallResult{}, err
	}
	httpRequest.Header.Set("Accept", "application/octet-stream, application/vnd.appimage")
	response, err := clone.Do(httpRequest)
	if err != nil {
		return InstallResult{}, fmt.Errorf("download official WeChat client: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return InstallResult{}, fmt.Errorf("official client download returned HTTP %d", response.StatusCode)
	}

	maximum := request.MaximumBytes
	if maximum <= 0 {
		maximum = defaultMaximumClientBytes
	}
	if response.ContentLength > maximum {
		return InstallResult{}, fmt.Errorf("official client is larger than configured limit (%d bytes)", maximum)
	}
	file, err := os.CreateTemp(filepath.Dir(destination), ".wechat-client-*.part")
	if err != nil {
		return InstallResult{}, err
	}
	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()
	if err := file.Chmod(0o700); err != nil {
		_ = file.Close()
		return InstallResult{}, err
	}

	hasher := sha256.New()
	written, copyErr := copyBounded(file, response.Body, hasher, maximum)
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr := file.Close()
	if copyErr != nil {
		return InstallResult{}, copyErr
	}
	if closeErr != nil {
		return InstallResult{}, closeErr
	}
	actual := hasher.Sum(nil)
	if subtle.ConstantTimeCompare(actual, expected) != 1 {
		return InstallResult{}, fmt.Errorf("official client checksum mismatch: got %s", hex.EncodeToString(actual))
	}
	if _, err := os.Lstat(destination); err == nil {
		return InstallResult{}, fmt.Errorf("refusing to overwrite existing client at %q", destination)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return InstallResult{}, err
	}
	if err := os.Rename(temporary, destination); err != nil {
		return InstallResult{}, err
	}
	return InstallResult{
		Path:      destination,
		SHA256:    hex.EncodeToString(actual),
		Size:      written,
		SourceURL: response.Request.URL.String(),
	}, nil
}

func allowedHostSet(hosts []string) map[string]struct{} {
	if len(hosts) == 0 {
		result := make(map[string]struct{}, len(defaultOfficialHosts))
		for host := range defaultOfficialHosts {
			result[host] = struct{}{}
		}
		return result
	}
	result := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if host != "" {
			result[host] = struct{}{}
		}
	}
	return result
}

func validateDownloadURL(raw string, allowed map[string]struct{}, allowHTTP bool) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse official client URL: %w", err)
	}
	if parsed.User != nil || parsed.Hostname() == "" {
		return nil, errors.New("official client URL must not contain credentials and must have a host")
	}
	if parsed.Scheme != "https" && !(allowHTTP && parsed.Scheme == "http") {
		return nil, errors.New("official client URL must use HTTPS")
	}
	if _, ok := allowed[strings.ToLower(parsed.Hostname())]; !ok {
		return nil, fmt.Errorf("client download host %q is not allow-listed", parsed.Hostname())
	}
	return parsed, nil
}

func parseSHA256(value string) ([]byte, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != sha256.Size*2 {
		return nil, errors.New("an exact 64-character SHA-256 is required")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return nil, errors.New("SHA-256 must be hexadecimal")
	}
	return decoded, nil
}

func matchingExistingClient(path string, expected []byte) (InstallResult, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return InstallResult{}, false, nil
	}
	if err != nil {
		return InstallResult{}, false, err
	}
	if !info.Mode().IsRegular() {
		return InstallResult{}, false, fmt.Errorf("client path %q is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return InstallResult{}, false, err
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil {
		return InstallResult{}, false, copyErr
	}
	if closeErr != nil {
		return InstallResult{}, false, closeErr
	}
	actual := hasher.Sum(nil)
	if subtle.ConstantTimeCompare(actual, expected) != 1 {
		return InstallResult{}, false, fmt.Errorf("existing client at %q has a different checksum", path)
	}
	return InstallResult{Path: path, SHA256: hex.EncodeToString(actual), Size: info.Size()}, true, nil
}

func copyBounded(destination io.Writer, source io.Reader, hasher hash.Hash, maximum int64) (int64, error) {
	limited := io.LimitReader(source, maximum+1)
	written, err := io.Copy(io.MultiWriter(destination, hasher), limited)
	if err != nil {
		return written, err
	}
	if written > maximum {
		return written, fmt.Errorf("official client exceeded configured limit (%d bytes)", maximum)
	}
	return written, nil
}
