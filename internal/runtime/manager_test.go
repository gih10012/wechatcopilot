package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gih10012/wechatcopilot/internal/account"
	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/gih10012/wechatcopilot/internal/driver"
	"github.com/gih10012/wechatcopilot/internal/driver/fake"
)

func TestActivateKeepsPreviousAccountWhenStopFails(t *testing.T) {
	store := testAccountStore(t)
	first, err := store.Add("first", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Add("second", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	var firstDriver *stopDriver
	manager.Register(driver.PlatformWeChat, func(runtime driver.AccountRuntime) (driver.Driver, error) {
		instance := &stopDriver{Driver: fake.New(driver.PlatformWeChat), failStop: runtime.AccountID == first.ID}
		if runtime.AccountID == first.ID {
			firstDriver = instance
		}
		return instance, nil
	})
	if _, err := manager.Activate(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(context.Background(), second.ID); err == nil {
		t.Fatal("activation succeeded even though the previous official client did not stop")
	}
	firstState, err := store.Resolve(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondState, err := store.Resolve(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !firstState.Active || secondState.Active {
		t.Fatalf("registry switched after failed stop: first=%#v second=%#v", firstState, secondState)
	}
	if _, _, err := manager.Driver(first.ID); err != nil {
		t.Fatalf("previous runtime was discarded: %v", err)
	}
	firstDriver.failStop = false
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPlatformActivationsAreSerialized(t *testing.T) {
	store := testAccountStore(t)
	first, _ := store.Add("first", driver.PlatformWeChat)
	second, _ := store.Add("second", driver.PlatformWeChat)
	manager := NewManager(store)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var once sync.Once
	manager.Register(driver.PlatformWeChat, func(runtime driver.AccountRuntime) (driver.Driver, error) {
		base := fake.New(driver.PlatformWeChat)
		if runtime.AccountID == first.ID {
			return &startDriver{Driver: base, start: func() {
				once.Do(func() { close(firstStarted) })
				<-releaseFirst
			}}, nil
		}
		return base, nil
	})
	firstDone := make(chan error, 1)
	go func() {
		_, err := manager.Activate(context.Background(), first.ID)
		firstDone <- err
	}()
	<-firstStarted
	secondDone := make(chan error, 1)
	go func() {
		_, err := manager.Activate(context.Background(), second.ID)
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("second activation bypassed platform lock: %v", err)
	default:
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	active, err := store.Resolve(second.ID)
	if err != nil || !active.Active {
		t.Fatalf("second account was not activated: %#v err=%v", active, err)
	}
	_ = manager.Shutdown(context.Background())
}

func TestDeactivateWaitsForIngestLoop(t *testing.T) {
	store := testAccountStore(t)
	item, err := store.Add("first", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	ingestStarted := make(chan struct{})
	ingestStopped := make(chan struct{})
	instance := &blockingIngestDriver{
		Driver:        fake.New(driver.PlatformWeChat),
		ingestStarted: ingestStarted,
		ingestStopped: ingestStopped,
	}
	manager.Register(driver.PlatformWeChat, func(driver.AccountRuntime) (driver.Driver, error) {
		return instance, nil
	})
	if _, err := manager.Activate(context.Background(), item.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ingestStarted:
	case <-time.After(time.Second):
		t.Fatal("ingest loop did not start")
	}
	if _, err := manager.Deactivate(context.Background(), item.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ingestStopped:
	default:
		t.Fatal("deactivate returned before the ingest loop stopped")
	}
}

func TestShutdownStopsPlatformRuntimesConcurrently(t *testing.T) {
	store := testAccountStore(t)
	wechat, _ := store.Add("personal", driver.PlatformWeChat)
	wecom, _ := store.Add("work", driver.PlatformWeCom)
	manager := NewManager(store)
	entered := make(chan driver.Platform, 2)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	for _, platform := range []driver.Platform{driver.PlatformWeChat, driver.PlatformWeCom} {
		platform := platform
		manager.Register(platform, func(driver.AccountRuntime) (driver.Driver, error) {
			return &coordinatedStopDriver{Driver: fake.New(platform), entered: entered, release: release}, nil
		})
	}
	if _, err := manager.Activate(context.Background(), wechat.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(context.Background(), wecom.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- manager.Shutdown(context.Background()) }()
	seen := make(map[driver.Platform]bool, 2)
	for len(seen) < 2 {
		select {
		case platform := <-entered:
			seen[platform] = true
		case <-time.After(time.Second):
			t.Fatalf("shutdown stopped runtimes serially; entered=%v", seen)
		}
	}
	close(release)
	released = true
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestShutdownDeadlineBoundsUnresponsiveIngestLoop(t *testing.T) {
	store := testAccountStore(t)
	item, _ := store.Add("personal", driver.PlatformWeChat)
	manager := NewManager(store)
	started := make(chan struct{})
	release := make(chan struct{})
	instance := &stubbornIngestDriver{
		Driver: fake.New(driver.PlatformWeChat), started: started, release: release,
	}
	manager.Register(driver.PlatformWeChat, func(driver.AccountRuntime) (driver.Driver, error) {
		return instance, nil
	})
	if _, err := manager.Activate(context.Background(), item.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("ingest loop did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := manager.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want deadline exceeded", err)
	}
	close(release)
}

func TestShutdownDeadlineBoundsUnresponsiveStop(t *testing.T) {
	store := testAccountStore(t)
	item, _ := store.Add("personal", driver.PlatformWeChat)
	manager := NewManager(store)
	started := make(chan struct{})
	release := make(chan struct{})
	instance := &stubbornStopDriver{
		Driver: fake.New(driver.PlatformWeChat), started: started, release: release,
	}
	manager.Register(driver.PlatformWeChat, func(driver.AccountRuntime) (driver.Driver, error) {
		return instance, nil
	})
	if _, err := manager.Activate(context.Background(), item.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- manager.Shutdown(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stop did not start")
	}
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want deadline exceeded", err)
	}
	close(release)
}

func TestShutdownDeadlineBoundsOccupiedPlatformLock(t *testing.T) {
	store := testAccountStore(t)
	manager := NewManager(store)
	operation := manager.operations[driver.PlatformWeChat]
	operation.Lock()
	defer operation.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := manager.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want deadline exceeded", err)
	}
}

func TestLockOperationRejectsCanceledContextWhenUnlocked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var operation sync.Mutex
	if err := lockOperation(ctx, &operation); !errors.Is(err, context.Canceled) {
		t.Fatalf("lockOperation error = %v, want context canceled", err)
	}
	if !operation.TryLock() {
		t.Fatal("canceled lockOperation acquired the mutex")
	}
	operation.Unlock()
}

func TestDeletingAccountCannotStopActivePlatformRuntime(t *testing.T) {
	store := testAccountStore(t)
	active, err := store.Add("active", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	deleting, err := store.Add("deleting", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	var activeDriver *stopDriver
	manager.Register(driver.PlatformWeChat, func(runtime driver.AccountRuntime) (driver.Driver, error) {
		instance := &stopDriver{Driver: fake.New(driver.PlatformWeChat), failStop: runtime.AccountID == active.ID}
		if runtime.AccountID == active.ID {
			activeDriver = instance
		}
		return instance, nil
	})
	if _, err := manager.Activate(context.Background(), active.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.BeginDelete(deleting.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(context.Background(), deleting.ID); !errors.Is(err, account.ErrDeleting) {
		t.Fatalf("deleting activation error = %v, want ErrDeleting", err)
	}
	if _, instance, err := manager.Driver(active.ID); err != nil || instance != activeDriver {
		t.Fatalf("active runtime was disturbed: instance=%T err=%v", instance, err)
	}
	activeDriver.failStop = false
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestActivateRollsBackWhenRuntimeDirectoryCannotBeCreated(t *testing.T) {
	store := testAccountStore(t)
	item, err := store.Add("first", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig := store.Runtime(item)
	if err := os.WriteFile(filepath.Dir(runtimeConfig.RuntimeDir), []byte("blocks account runtime directories"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	var factoryCalls atomic.Int32
	manager.Register(driver.PlatformWeChat, func(driver.AccountRuntime) (driver.Driver, error) {
		factoryCalls.Add(1)
		return fake.New(driver.PlatformWeChat), nil
	})

	if _, err := manager.Activate(context.Background(), item.ID); err == nil {
		t.Fatal("activation succeeded with an unusable runtime directory")
	}
	rolledBack, err := store.Resolve(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Active || rolledBack.State != driver.StateStopped || rolledBack.LastError == "" {
		t.Fatalf("failed activation remained active or lost its failure status: %#v", rolledBack)
	}
	if got := factoryCalls.Load(); got != 0 {
		t.Fatalf("driver factory called %d times before runtime directory creation", got)
	}
	if _, _, err := manager.Driver(item.ID); err == nil {
		t.Fatal("failed activation installed a runtime driver")
	}
}

func TestRestorePreservesRequestedSlotAfterTransientStartFailure(t *testing.T) {
	store := testAccountStore(t)
	item, err := store.Add("first", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Activate(item.ID); err != nil {
		t.Fatal(err)
	}

	failing := NewManager(store)
	failing.Register(driver.PlatformWeChat, func(driver.AccountRuntime) (driver.Driver, error) {
		return nil, errors.New("temporary Docker outage")
	})
	restoreErrors := failing.Restore(context.Background())
	if len(restoreErrors) != 1 {
		t.Fatalf("restore errors = %v, want one transient failure", restoreErrors)
	}
	preserved, err := store.Resolve(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !preserved.Active || preserved.State != driver.StateDegraded || preserved.LastError == "" {
		t.Fatalf("restore failure discarded requested slot or status: %#v", preserved)
	}
	recovered := NewManager(store)
	recovered.Register(driver.PlatformWeChat, func(driver.AccountRuntime) (driver.Driver, error) {
		return fake.New(driver.PlatformWeChat), nil
	})
	if restoreErrors := recovered.Restore(context.Background()); len(restoreErrors) != 0 {
		t.Fatalf("second restore did not recover: %v", restoreErrors)
	}
	if _, _, err := recovered.Driver(item.ID); err != nil {
		t.Fatalf("requested slot was not restored after dependency recovery: %v", err)
	}
	if err := recovered.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitActivateRetriesPreservedRestoreSlot(t *testing.T) {
	store := testAccountStore(t)
	item, err := store.Add("first", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Activate(item.ID); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(store)
	manager.Register(driver.PlatformWeChat, func(driver.AccountRuntime) (driver.Driver, error) {
		return nil, errors.New("temporary Docker outage")
	})
	if restoreErrors := manager.Restore(context.Background()); len(restoreErrors) != 1 {
		t.Fatalf("restore errors = %v, want one transient failure", restoreErrors)
	}
	manager.Register(driver.PlatformWeChat, func(driver.AccountRuntime) (driver.Driver, error) {
		return fake.New(driver.PlatformWeChat), nil
	})
	if _, err := manager.Activate(context.Background(), item.ID); err != nil {
		t.Fatalf("explicit activation did not retry the requested slot: %v", err)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type stopDriver struct {
	*fake.Driver
	failStop bool
}

type coordinatedStopDriver struct {
	*fake.Driver
	entered chan<- driver.Platform
	release <-chan struct{}
}

type stubbornIngestDriver struct {
	*fake.Driver
	started chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (d *stubbornIngestDriver) ListConversations(ctx context.Context, _ driver.ConversationQuery) ([]driver.Conversation, error) {
	d.once.Do(func() { close(d.started) })
	<-d.release
	return nil, ctx.Err()
}

type stubbornStopDriver struct {
	*fake.Driver
	started chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (d *stubbornStopDriver) Stop(context.Context) error {
	d.once.Do(func() { close(d.started) })
	<-d.release
	return nil
}

func (d *coordinatedStopDriver) Stop(ctx context.Context) error {
	d.entered <- d.Platform()
	select {
	case <-d.release:
		return d.Driver.Stop(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *stopDriver) Stop(ctx context.Context) error {
	if d.failStop {
		return errors.New("fixture stop failed")
	}
	return d.Driver.Stop(ctx)
}

type startDriver struct {
	*fake.Driver
	start func()
}

type blockingIngestDriver struct {
	*fake.Driver
	ingestStarted chan struct{}
	ingestStopped chan struct{}
	started       atomic.Bool
}

func (d *blockingIngestDriver) ListConversations(ctx context.Context, _ driver.ConversationQuery) ([]driver.Conversation, error) {
	if d.started.CompareAndSwap(false, true) {
		close(d.ingestStarted)
	}
	<-ctx.Done()
	close(d.ingestStopped)
	return nil, ctx.Err()
}

func (d *startDriver) Start(ctx context.Context, runtime driver.AccountRuntime) error {
	d.start()
	return d.Driver.Start(ctx, runtime)
}

func testAccountStore(t *testing.T) *account.Store {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "state")
	runtimeDir := filepath.Join(root, "runtime")
	store, err := account.Open(config.Paths{
		Home: home, Accounts: filepath.Join(home, "accounts"), Downloads: filepath.Join(home, "downloads"),
		Runtime: runtimeDir, Socket: filepath.Join(runtimeDir, "daemon.sock"), Registry: filepath.Join(home, "accounts.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}
