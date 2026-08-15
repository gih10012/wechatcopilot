// Package app wires configured drivers into the daemon without putting
// platform-specific dependencies in the CLI or service packages.
package app

import (
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/gih10012/wechatcopilot/internal/driver"
	"github.com/gih10012/wechatcopilot/internal/driver/fake"
	"github.com/gih10012/wechatcopilot/internal/driver/wechat"
	"github.com/gih10012/wechatcopilot/internal/driver/wecom"
)

const defaultWeChatImage = "wechatcopilot/wechat-runtime:v0.1.0"

func DriverFactories(paths config.Paths) map[driver.Platform]driver.Factory {
	if enabled("WECHATCOPILOT_FAKE_DRIVERS") {
		return map[driver.Platform]driver.Factory{
			driver.PlatformWeChat: func(driver.AccountRuntime) (driver.Driver, error) {
				return fake.New(driver.PlatformWeChat), nil
			},
			driver.PlatformWeCom: func(driver.AccountRuntime) (driver.Driver, error) {
				return fake.New(driver.PlatformWeCom), nil
			},
		}
	}

	wechatImage := valueOr("WECHATCOPILOT_WECHAT_IMAGE", defaultWeChatImage)
	wechatAppImage := valueOr("WECHATCOPILOT_WECHAT_APPIMAGE", filepath.Join(paths.Downloads, "WeChat.AppImage"))
	wechatFactory := wechat.NewFactory(wechat.Config{
		Docker: wechat.DockerConfig{
			Binary:                 valueOr("WECHATCOPILOT_DOCKER", "docker"),
			Image:                  wechatImage,
			AppImagePath:           wechatAppImage,
			ExpectedAppImageSHA256: os.Getenv(config.EnvWeChatAppImageSHA256),
		},
	})

	wecomConfig := wecom.DefaultConfig()
	wecomConfig.DockerBinary = valueOr("WECHATCOPILOT_DOCKER", wecomConfig.DockerBinary)
	wecomConfig.RedroidImage = os.Getenv("WECHATCOPILOT_WECOM_REDROID_IMAGE")
	wecomConfig.OfficialAPKURL = os.Getenv("WECHATCOPILOT_WECOM_APK_URL")
	wecomConfig.OfficialAPKSHA256 = os.Getenv("WECHATCOPILOT_WECOM_APK_SHA256")
	wecomConfig.OfficialAPKPath = valueOr("WECHATCOPILOT_WECOM_APK", filepath.Join(paths.Downloads, "WeCom.apk"))
	wecomConfig.CompanionAPKPath = valueOr("WECHATCOPILOT_WECOM_COMPANION_APK", filepath.Join(paths.Downloads, "wechatcopilot-companion.apk"))
	if timeout, err := time.ParseDuration(os.Getenv("WECHATCOPILOT_DRIVER_STARTUP_TIMEOUT")); err == nil && timeout > 0 {
		wecomConfig.StartupTimeout = timeout
	}

	return map[driver.Platform]driver.Factory{
		driver.PlatformWeChat: wechatFactory,
		driver.PlatformWeCom:  wecom.Factory(wecomConfig, nil),
	}
}

func valueOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func enabled(name string) bool {
	value, err := strconv.ParseBool(os.Getenv(name))
	return err == nil && value
}
