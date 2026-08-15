package fake

import (
	"context"
	"testing"

	"github.com/gih10012/wechatcopilot/internal/driver"
)

func TestCapabilitiesUseSharedCompleteContract(t *testing.T) {
	instance := New(driver.PlatformWeChat)
	if err := instance.Start(context.Background(), driver.AccountRuntime{AccountID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	status, err := instance.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.ValidateCapabilities(status.Capabilities); err != nil {
		t.Fatal(err)
	}
}
