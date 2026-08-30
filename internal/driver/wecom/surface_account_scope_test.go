package wecom

import (
	"context"
	"errors"
	"testing"

	core "github.com/gih10012/wechatcopilot/internal/driver"
)

func TestNotificationSurfaceReferenceRejectsAnotherAccountWithSameSequence(t *testing.T) {
	const sequence = int64(7)
	accountAReference := notificationSurfaceReference("account-a", sequence)
	accountBReference := notificationSurfaceReference("account-b", sequence)
	if accountAReference == accountBReference {
		t.Fatal("notification surface reference is not account scoped")
	}
	driverB := &Driver{
		runtime: &Runtime{}, account: core.AccountRuntime{AccountID: "account-b"},
		surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo),
	}
	if _, err := driverB.OpenSurface(context.Background(), accountAReference); !errors.Is(err, ErrNotFound) {
		t.Fatalf("account A reference used by account B error = %v, want ErrNotFound", err)
	}
}
