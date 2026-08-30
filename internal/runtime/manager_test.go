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
	waitForAtomicAtLeast(t, &firstDriver.listCalls, 1)
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
	waitForAtomicAtLeast(t, &firstDriver.listCalls, 2)
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
	if instance.stopBeforeIngestExit.Load() {
		t.Fatal("driver.Stop overlapped ListConversations")
	}
}

func TestDeactivateWaitsForReadMessages(t *testing.T) {
	store := testAccountStore(t)
	item, err := store.Add("first", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	instance := &blockingReadDriver{
		Driver: fake.New(driver.PlatformWeChat), started: make(chan struct{}), exited: make(chan struct{}),
	}
	activated, _, err := store.Activate(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.Start(context.Background(), store.Runtime(activated)); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	runtime := &activeRuntime{
		account: activated, driver: instance, cancel: cancel, done: make(chan struct{}),
	}
	manager.active[driver.PlatformWeChat] = runtime
	go func() {
		defer close(runtime.done)
		runtime.ingestMu.Lock()
		defer runtime.ingestMu.Unlock()
		_, _ = instance.ReadMessages(runCtx, driver.MessageQuery{})
	}()
	select {
	case <-instance.started:
	case <-time.After(time.Second):
		t.Fatal("ReadMessages did not start")
	}
	if _, err := manager.Deactivate(context.Background(), item.ID); err != nil {
		t.Fatal(err)
	}
	if instance.stopBeforeIngestExit.Load() {
		t.Fatal("driver.Stop overlapped ReadMessages")
	}
}

func TestAccountSwitchWaitsForPreviousIngestLoop(t *testing.T) {
	store := testAccountStore(t)
	first, _ := store.Add("first", driver.PlatformWeChat)
	second, _ := store.Add("second", driver.PlatformWeChat)
	manager := NewManager(store)
	previous := &blockingIngestDriver{
		Driver: fake.New(driver.PlatformWeChat), ingestStarted: make(chan struct{}), ingestStopped: make(chan struct{}),
	}
	manager.Register(driver.PlatformWeChat, func(runtime driver.AccountRuntime) (driver.Driver, error) {
		if runtime.AccountID == first.ID {
			return previous, nil
		}
		return fake.New(driver.PlatformWeChat), nil
	})
	if _, err := manager.Activate(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-previous.ingestStarted:
	case <-time.After(time.Second):
		t.Fatal("previous account ingest loop did not start")
	}
	if _, err := manager.Activate(context.Background(), second.ID); err != nil {
		t.Fatal(err)
	}
	if previous.stopBeforeIngestExit.Load() {
		t.Fatal("account switch called driver.Stop before the previous ingest loop exited")
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDeactivateWaitsForExplicitSyncBeforeStop(t *testing.T) {
	store := testAccountStore(t)
	item, _ := store.Add("first", driver.PlatformWeChat)
	manager := NewManager(store)
	instance := &blockingSyncDriver{
		Driver: fake.New(driver.PlatformWeChat), initialDone: make(chan struct{}),
		syncStarted: make(chan struct{}), releaseSync: make(chan struct{}), stopCalled: make(chan struct{}),
	}
	manager.Register(driver.PlatformWeChat, func(driver.AccountRuntime) (driver.Driver, error) {
		return instance, nil
	})
	if _, err := manager.Activate(context.Background(), item.ID); err != nil {
		t.Fatal(err)
	}
	manager.mu.RLock()
	backgroundDone := manager.active[driver.PlatformWeChat].done
	manager.mu.RUnlock()
	select {
	case <-instance.initialDone:
	case <-time.After(time.Second):
		t.Fatal("initial ingest did not finish")
	}
	instance.blockSync.Store(true)
	syncDone := make(chan error, 1)
	go func() { syncDone <- manager.Sync(context.Background(), item.ID) }()
	select {
	case <-instance.syncStarted:
	case <-time.After(time.Second):
		t.Fatal("explicit Sync did not enter ReadMessages")
	}
	deactivateDone := make(chan error, 1)
	go func() {
		_, err := manager.Deactivate(context.Background(), item.ID)
		deactivateDone <- err
	}()
	select {
	case <-backgroundDone:
	case <-time.After(time.Second):
		t.Fatal("deactivation did not drain the background ingest loop")
	}
	close(instance.releaseSync)
	if err := <-syncDone; err != nil {
		t.Fatalf("explicit Sync failed: %v", err)
	}
	if err := <-deactivateDone; err != nil {
		t.Fatalf("deactivate after Sync: %v", err)
	}
	if instance.stopOverlapped.Load() {
		t.Fatal("driver.Stop observed an in-flight ReadMessages call")
	}
	select {
	case <-instance.stopCalled:
	default:
		t.Fatal("deactivation returned without calling driver.Stop")
	}
}

func TestShutdownStopsPlatformRuntimesConcurrently(t *testing.T) {
	store := testAccountStore(t)
	wechat, _ := store.Add("personal", driver.PlatformWeChat)
	wecom, _ := store.Add("work", driver.PlatformWeCom)
	manager := NewManager(store)
	ingestStarted := make(chan driver.Platform, 2)
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
			return &coordinatedStopDriver{
				Driver: fake.New(platform), ingestStarted: ingestStarted, ingestExited: make(chan struct{}),
				entered: entered, release: release,
			}, nil
		})
	}
	if _, err := manager.Activate(context.Background(), wechat.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(context.Background(), wecom.ID); err != nil {
		t.Fatal(err)
	}
	started := make(map[driver.Platform]bool, 2)
	for len(started) < 2 {
		select {
		case platform := <-ingestStarted:
			started[platform] = true
		case <-time.After(time.Second):
			t.Fatalf("ingest loops did not start: %v", started)
		}
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
	if calls := instance.stopCalls.Load(); calls != 0 {
		t.Fatalf("driver.Stop called %d times before the ingest loop exited", calls)
	}
	close(release)
}

func TestDeactivateDeadlineBoundsUnresponsiveIngestWithoutStoppingDriver(t *testing.T) {
	store := testAccountStore(t)
	item, _ := store.Add("personal", driver.PlatformWeChat)
	manager := NewManager(store)
	instance := &stubbornIngestDriver{
		Driver: fake.New(driver.PlatformWeChat), started: make(chan struct{}), release: make(chan struct{}),
	}
	manager.Register(driver.PlatformWeChat, func(driver.AccountRuntime) (driver.Driver, error) {
		return instance, nil
	})
	if _, err := manager.Activate(context.Background(), item.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-instance.started:
	case <-time.After(time.Second):
		t.Fatal("ingest loop did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := manager.Deactivate(ctx, item.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Deactivate error = %v, want deadline exceeded", err)
	}
	if calls := instance.stopCalls.Load(); calls != 0 {
		t.Fatalf("driver.Stop called %d times before the ingest loop exited", calls)
	}
	preserved, err := store.Resolve(item.ID)
	if err != nil || !preserved.Active {
		t.Fatalf("timed-out deactivation changed the active slot: %#v err=%v", preserved, err)
	}
	close(instance.release)
	waitForAtomicAtLeast(t, &instance.listCalls, 2)
	if _, err := manager.Deactivate(context.Background(), item.ID); err != nil {
		t.Fatalf("deactivation retry after ingest exit: %v", err)
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
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

func TestRepeatedRestoreRecoversTransientFactoryFailure(t *testing.T) {
	store := testAccountStore(t)
	item, err := store.Add("first", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Activate(item.ID); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	var factoryCalls atomic.Int32
	temporary := errors.New("temporary Docker outage")
	manager.Register(driver.PlatformWeChat, func(driver.AccountRuntime) (driver.Driver, error) {
		if factoryCalls.Add(1) < 3 {
			return nil, temporary
		}
		return fake.New(driver.PlatformWeChat), nil
	})
	for attempt := 1; attempt <= 2; attempt++ {
		restoreErrors := manager.Restore(context.Background())
		if len(restoreErrors) != 1 || !errors.Is(restoreErrors[0], temporary) {
			t.Fatalf("restore attempt %d errors = %v, want transient failure", attempt, restoreErrors)
		}
	}
	if restoreErrors := manager.Restore(context.Background()); len(restoreErrors) != 0 {
		t.Fatalf("third restore did not recover: %v", restoreErrors)
	}
	if _, _, err := manager.Driver(item.ID); err != nil {
		t.Fatalf("recovered runtime is unavailable: %v", err)
	}
	if calls := factoryCalls.Load(); calls != 3 {
		t.Fatalf("driver factory calls = %d, want 3", calls)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatedRestoreRechecksTransientStatusFailure(t *testing.T) {
	store := testAccountStore(t)
	item, err := store.Add("first", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Activate(item.ID); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	var factoryCalls atomic.Int32
	temporary := errors.New("temporary driver probe outage")
	instance := &transientStatusDriver{Driver: fake.New(driver.PlatformWeChat), failure: temporary, failures: 2}
	manager.Register(driver.PlatformWeChat, func(driver.AccountRuntime) (driver.Driver, error) {
		factoryCalls.Add(1)
		return instance, nil
	})
	for attempt := 1; attempt <= 2; attempt++ {
		restoreErrors := manager.Restore(context.Background())
		if len(restoreErrors) != 1 || !errors.Is(restoreErrors[0], temporary) {
			t.Fatalf("restore attempt %d errors = %v, want transient status failure", attempt, restoreErrors)
		}
	}
	if restoreErrors := manager.Restore(context.Background()); len(restoreErrors) != 0 {
		t.Fatalf("third status check did not recover: %v", restoreErrors)
	}
	if calls := factoryCalls.Load(); calls != 1 {
		t.Fatalf("status retry recreated the driver %d times", calls)
	}
	resolved, err := store.Resolve(item.ID)
	if err != nil || resolved.State != driver.StateOnline || resolved.LastError != "" {
		t.Fatalf("successful status retry did not clear degraded state: %#v err=%v", resolved, err)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
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

func TestRestoreActivationDoesNotReactivateExplicitlyDeactivatedAccount(t *testing.T) {
	store := testAccountStore(t)
	item, err := store.Add("first", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Activate(item.ID); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	var factoryCalls atomic.Int32
	manager.Register(driver.PlatformWeChat, func(driver.AccountRuntime) (driver.Driver, error) {
		factoryCalls.Add(1)
		return fake.New(driver.PlatformWeChat), nil
	})
	if _, err := store.Deactivate(item.ID); err != nil {
		t.Fatal(err)
	}
	if restored, err := manager.activate(context.Background(), item.ID, true); err != nil {
		t.Fatalf("stale restore activation returned an error: %v", err)
	} else if restored.Active {
		t.Fatalf("stale restore activation returned an active account: %#v", restored)
	}
	if calls := factoryCalls.Load(); calls != 0 {
		t.Fatalf("stale restore invoked the driver factory %d times", calls)
	}
	persisted, err := store.Resolve(item.ID)
	if err != nil || persisted.Active {
		t.Fatalf("stale restore revived an explicitly deactivated account: %#v err=%v", persisted, err)
	}
}

type stopDriver struct {
	*fake.Driver
	failStop  bool
	listCalls atomic.Int32
}

type transientStatusDriver struct {
	*fake.Driver
	calls    atomic.Int32
	failure  error
	failures int32
}

func (d *transientStatusDriver) Status(ctx context.Context) (driver.Status, error) {
	if d.calls.Add(1) <= d.failures {
		return driver.Status{}, d.failure
	}
	return d.Driver.Status(ctx)
}

type coordinatedStopDriver struct {
	*fake.Driver
	ingestStarted        chan<- driver.Platform
	ingestExited         chan struct{}
	ingestOnce           sync.Once
	stopBeforeIngestExit atomic.Bool
	entered              chan<- driver.Platform
	release              <-chan struct{}
}

type stubbornIngestDriver struct {
	*fake.Driver
	started   chan struct{}
	release   chan struct{}
	once      sync.Once
	listCalls atomic.Int32
	stopCalls atomic.Int32
}

func (d *stubbornIngestDriver) ListConversations(ctx context.Context, _ driver.ConversationQuery) ([]driver.Conversation, error) {
	d.listCalls.Add(1)
	d.once.Do(func() { close(d.started) })
	<-d.release
	return nil, ctx.Err()
}

func (d *stubbornIngestDriver) Stop(ctx context.Context) error {
	d.stopCalls.Add(1)
	return d.Driver.Stop(ctx)
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
	select {
	case <-d.ingestExited:
	default:
		d.stopBeforeIngestExit.Store(true)
		return errors.New("Stop entered before ingest exited")
	}
	d.entered <- d.Platform()
	select {
	case <-d.release:
		return d.Driver.Stop(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *coordinatedStopDriver) ListConversations(ctx context.Context, _ driver.ConversationQuery) ([]driver.Conversation, error) {
	d.ingestOnce.Do(func() { d.ingestStarted <- d.Platform() })
	<-ctx.Done()
	close(d.ingestExited)
	return nil, ctx.Err()
}

func (d *stopDriver) Stop(ctx context.Context) error {
	if d.failStop {
		return errors.New("fixture stop failed")
	}
	return d.Driver.Stop(ctx)
}

func (d *stopDriver) ListConversations(ctx context.Context, query driver.ConversationQuery) ([]driver.Conversation, error) {
	d.listCalls.Add(1)
	return d.Driver.ListConversations(ctx, query)
}

type startDriver struct {
	*fake.Driver
	start func()
}

type blockingIngestDriver struct {
	*fake.Driver
	ingestStarted        chan struct{}
	ingestStopped        chan struct{}
	started              atomic.Bool
	stopBeforeIngestExit atomic.Bool
}

func (d *blockingIngestDriver) ListConversations(ctx context.Context, _ driver.ConversationQuery) ([]driver.Conversation, error) {
	if d.started.CompareAndSwap(false, true) {
		close(d.ingestStarted)
	}
	<-ctx.Done()
	close(d.ingestStopped)
	return nil, ctx.Err()
}

func (d *blockingIngestDriver) Stop(ctx context.Context) error {
	select {
	case <-d.ingestStopped:
	default:
		d.stopBeforeIngestExit.Store(true)
		return errors.New("Stop entered before ListConversations exited")
	}
	return d.Driver.Stop(ctx)
}

type blockingReadDriver struct {
	*fake.Driver
	started              chan struct{}
	exited               chan struct{}
	once                 sync.Once
	stopBeforeIngestExit atomic.Bool
}

type blockingSyncDriver struct {
	*fake.Driver
	initialDone    chan struct{}
	initialOnce    sync.Once
	blockSync      atomic.Bool
	syncStarted    chan struct{}
	syncStartOnce  sync.Once
	releaseSync    chan struct{}
	activeReads    atomic.Int32
	stopCalled     chan struct{}
	stopOnce       sync.Once
	stopOverlapped atomic.Bool
}

func (d *blockingReadDriver) ReadMessages(ctx context.Context, _ driver.MessageQuery) ([]driver.Message, error) {
	d.once.Do(func() { close(d.started) })
	<-ctx.Done()
	close(d.exited)
	return nil, ctx.Err()
}

func (d *blockingReadDriver) Stop(ctx context.Context) error {
	select {
	case <-d.exited:
	default:
		d.stopBeforeIngestExit.Store(true)
		return errors.New("Stop entered before ReadMessages exited")
	}
	return d.Driver.Stop(ctx)
}

func (d *blockingSyncDriver) ReadMessages(ctx context.Context, query driver.MessageQuery) ([]driver.Message, error) {
	if !d.blockSync.Load() {
		messages, err := d.Driver.ReadMessages(ctx, query)
		d.initialOnce.Do(func() { close(d.initialDone) })
		return messages, err
	}
	d.activeReads.Add(1)
	defer d.activeReads.Add(-1)
	d.syncStartOnce.Do(func() { close(d.syncStarted) })
	select {
	case <-d.releaseSync:
		return d.Driver.ReadMessages(ctx, query)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *blockingSyncDriver) Stop(ctx context.Context) error {
	if d.activeReads.Load() != 0 {
		d.stopOverlapped.Store(true)
	}
	d.stopOnce.Do(func() { close(d.stopCalled) })
	return d.Driver.Stop(ctx)
}

func (d *startDriver) Start(ctx context.Context, runtime driver.AccountRuntime) error {
	d.start()
	return d.Driver.Start(ctx, runtime)
}

func waitForAtomicAtLeast(t *testing.T, value *atomic.Int32, minimum int32) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if value.Load() >= minimum {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("counter = %d, want at least %d", value.Load(), minimum)
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
