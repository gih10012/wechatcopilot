package app

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/gih10012/wechatcopilot/internal/driver"
)

func TestDriverFactoriesRequireWeChatAppImageDigest(t *testing.T) {
	t.Setenv("WECHATCOPILOT_FAKE_DRIVERS", "")
	t.Setenv(config.EnvWeChatAppImageSHA256, "")
	paths := config.Paths{Downloads: filepath.Join(t.TempDir(), "downloads")}

	factory := DriverFactories(paths)[driver.PlatformWeChat]
	if _, err := factory(driver.AccountRuntime{}); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("WeChat factory error without pinned digest = %v", err)
	}
}

func TestDriverFactoriesPassWeChatAppImageDigest(t *testing.T) {
	t.Setenv("WECHATCOPILOT_FAKE_DRIVERS", "")
	t.Setenv(config.EnvWeChatAppImageSHA256, strings.Repeat("A", 64))
	paths := config.Paths{Downloads: filepath.Join(t.TempDir(), "downloads")}

	factory := DriverFactories(paths)[driver.PlatformWeChat]
	if _, err := factory(driver.AccountRuntime{}); err != nil {
		t.Fatalf("WeChat factory rejected configured digest: %v", err)
	}
}
