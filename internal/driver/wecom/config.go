// Package wecom drives the official WeCom Android client in an isolated
// Redroid container. It never implements or proxies the WeCom network
// protocol.
package wecom

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	DefaultWeComPackage       = "com.tencent.wework"
	DefaultCompanionPackage   = "dev.wechatcopilot.companion"
	DefaultCompanionPort      = 18765
	defaultContainerStopGrace = 20 * time.Second
)

var (
	accountIDPattern   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
	sha256Pattern      = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
	imageDigestPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]*@sha256:[a-f0-9]{64}$`)
	packageNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*(\.[a-zA-Z][a-zA-Z0-9_]*)+$`)
)

// Config contains host-level settings. An image digest and APK hash are
// mandatory so a client update cannot silently change the runtime.
type Config struct {
	DockerBinary      string
	RedroidImage      string
	OfficialAPKURL    string
	OfficialAPKSHA256 string
	OfficialAPKPath   string
	CompanionAPKPath  string
	WeComPackage      string
	CompanionPackage  string
	StartupTimeout    time.Duration
	StopGrace         time.Duration
	HTTPTimeout       time.Duration
	DownloadTimeout   time.Duration
}

func DefaultConfig() Config {
	return Config{
		DockerBinary:     "docker",
		WeComPackage:     DefaultWeComPackage,
		CompanionPackage: DefaultCompanionPackage,
		StartupTimeout:   2 * time.Minute,
		StopGrace:        defaultContainerStopGrace,
		HTTPTimeout:      15 * time.Second,
		DownloadTimeout:  15 * time.Minute,
	}
}

func (c *Config) normalize() {
	defaults := DefaultConfig()
	if c.DockerBinary == "" {
		c.DockerBinary = defaults.DockerBinary
	}
	if c.WeComPackage == "" {
		c.WeComPackage = defaults.WeComPackage
	}
	if c.CompanionPackage == "" {
		c.CompanionPackage = defaults.CompanionPackage
	}
	if c.StartupTimeout == 0 {
		c.StartupTimeout = defaults.StartupTimeout
	}
	if c.StopGrace == 0 {
		c.StopGrace = defaults.StopGrace
	}
	if c.HTTPTimeout == 0 {
		c.HTTPTimeout = defaults.HTTPTimeout
	}
	if c.DownloadTimeout == 0 {
		c.DownloadTimeout = defaults.DownloadTimeout
	}
}

// Validate checks configuration without contacting Docker or downloading
// either official client artifact.
func (c Config) Validate() error {
	c.normalize()
	var problems []error
	if !imageDigestPattern.MatchString(c.RedroidImage) {
		problems = append(problems, errors.New("redroid image must be pinned by @sha256 digest"))
	}
	if c.OfficialAPKPath == "" {
		problems = append(problems, errors.New("official APK path is required"))
	}
	if !sha256Pattern.MatchString(c.OfficialAPKSHA256) {
		problems = append(problems, errors.New("official APK SHA-256 must contain 64 hexadecimal characters"))
	}
	if c.CompanionAPKPath == "" {
		problems = append(problems, errors.New("companion APK path is required"))
	}
	if !packageNamePattern.MatchString(c.WeComPackage) || !packageNamePattern.MatchString(c.CompanionPackage) {
		problems = append(problems, errors.New("Android package names are invalid"))
	}
	if c.OfficialAPKURL != "" {
		u, err := url.Parse(c.OfficialAPKURL)
		if err != nil || u.Scheme != "https" || !allowedAPKHost(u.Hostname()) {
			problems = append(problems, errors.New("official APK URL must use HTTPS on an approved Tencent host"))
		}
	}
	if c.StartupTimeout <= 0 || c.StopGrace <= 0 || c.HTTPTimeout <= 0 || c.DownloadTimeout <= 0 {
		problems = append(problems, errors.New("timeouts must be positive"))
	}
	return errors.Join(problems...)
}

func validateAccountID(id string) error {
	if !accountIDPattern.MatchString(id) {
		return fmt.Errorf("invalid account ID %q", id)
	}
	return nil
}

func accountDataDir(stateDir, accountID string) (string, error) {
	if err := validateAccountID(accountID); err != nil {
		return "", err
	}
	if !filepath.IsAbs(stateDir) {
		return "", errors.New("account state directory must be absolute")
	}
	stateDir = filepath.Clean(stateDir)
	if filepath.Base(stateDir) != accountID {
		return "", errors.New("account state directory is not bound to the account ID")
	}
	return filepath.Join(stateDir, "wecom", "android-data"), nil
}

func allowedAPKHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, suffix := range []string{"work.weixin.qq.com", "dldir1.qq.com", "qpic.cn"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}
