package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gih10012/wechatcopilot/internal/account"
	"github.com/gih10012/wechatcopilot/internal/driver"
	"github.com/gih10012/wechatcopilot/internal/index"
)

type activeRuntime struct {
	account  account.Account
	driver   driver.Driver
	cancel   context.CancelFunc
	done     chan struct{}
	ingestMu sync.Mutex
	cursor   int64
}

type Manager struct {
	mu         sync.RWMutex
	accounts   *account.Store
	factories  map[driver.Platform]driver.Factory
	active     map[driver.Platform]*activeRuntime
	operations map[driver.Platform]*sync.Mutex
	closed     bool
}

func NewManager(accounts *account.Store) *Manager {
	return &Manager{
		accounts: accounts, factories: make(map[driver.Platform]driver.Factory),
		active: make(map[driver.Platform]*activeRuntime),
		operations: map[driver.Platform]*sync.Mutex{
			driver.PlatformWeChat: {},
			driver.PlatformWeCom:  {},
		},
	}
}

func (m *Manager) Register(platform driver.Platform, factory driver.Factory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.factories[platform] = factory
}

func (m *Manager) Restore(ctx context.Context) []error {
	var result []error
	for _, item := range m.accounts.List() {
		if item.Active && !item.Deleting {
			if _, err := m.activate(ctx, item.ID, true); err != nil {
				result = append(result, fmt.Errorf("restore %s: %w", item.Alias, err))
			}
		}
	}
	return result
}

// BeginDelete serializes the durable deletion marker with platform lifecycle
// operations so a concurrent activation cannot pass the marker transition.
func (m *Manager) BeginDelete(value string) (account.Account, error) {
	target, err := m.accounts.Resolve(value)
	if err != nil {
		return account.Account{}, err
	}
	operation := m.operations[target.Platform]
	operation.Lock()
	defer operation.Unlock()
	target, err = m.accounts.Resolve(target.ID)
	if err != nil {
		return account.Account{}, err
	}
	return m.accounts.BeginDelete(target.ID)
}

func (m *Manager) Activate(ctx context.Context, value string) (account.Account, error) {
	return m.activate(ctx, value, false)
}

// activate preserves a previously requested active slot during daemon restore.
// A transient dependency failure must not turn an automatic restart into a
// durable deactivation; explicit activation failures still roll back normally.
func (m *Manager) activate(ctx context.Context, value string, restoring bool) (account.Account, error) {
	target, err := m.accounts.Resolve(value)
	if err != nil {
		return account.Account{}, err
	}
	if target.Deleting {
		return account.Account{}, account.ErrDeleting
	}
	operation := m.operations[target.Platform]
	if err := lockOperation(ctx, operation); err != nil {
		return account.Account{}, err
	}
	defer operation.Unlock()
	target, err = m.accounts.Resolve(target.ID)
	if err != nil {
		return account.Account{}, err
	}
	if target.Deleting {
		return account.Account{}, account.ErrDeleting
	}
	m.mu.Lock()
	factory := m.factories[target.Platform]
	previous := m.active[target.Platform]
	if m.closed {
		m.mu.Unlock()
		return account.Account{}, errors.New("runtime manager is closed")
	}
	if factory == nil {
		m.mu.Unlock()
		return account.Account{}, fmt.Errorf("no %s driver is installed", target.Platform)
	}
	if previous != nil && previous.account.ID == target.ID {
		m.mu.Unlock()
		return previous.account, nil
	}
	if previous != nil {
		if err := previous.driver.Stop(ctx); err != nil {
			m.mu.Unlock()
			return account.Account{}, fmt.Errorf("stop active %s account %s: %w", target.Platform, previous.account.Alias, err)
		}
		previous.cancel()
		delete(m.active, target.Platform)
	}
	m.mu.Unlock()
	if previous != nil {
		<-previous.done
	}

	activated, _, err := m.accounts.Activate(target.ID)
	if err != nil {
		return account.Account{}, err
	}
	runtimeConfig := m.accounts.Runtime(activated)
	if err := os.MkdirAll(runtimeConfig.RuntimeDir, 0o700); err != nil {
		m.recordActivationFailure(activated.ID, err, restoring)
		return account.Account{}, err
	}
	instance, err := factory(runtimeConfig)
	if err != nil {
		m.recordActivationFailure(activated.ID, err, restoring)
		return account.Account{}, err
	}
	if err := instance.Start(ctx, runtimeConfig); err != nil {
		m.recordActivationFailure(activated.ID, err, restoring)
		return account.Account{}, err
	}
	status, statusErr := instance.Status(ctx)
	if statusErr != nil {
		status = driver.Status{State: driver.StateDegraded, Reason: statusErr.Error(), ObservedAt: time.Now().UTC()}
	}
	_ = m.accounts.UpdateStatus(activated.ID, status)
	runCtx, cancel := context.WithCancel(context.Background())
	runtime := &activeRuntime{account: activated, driver: instance, cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	m.active[activated.Platform] = runtime
	m.mu.Unlock()
	go m.ingestLoop(runCtx, runtime)
	return m.accounts.Resolve(activated.ID)
}

func (m *Manager) recordActivationFailure(accountID string, failure error, restoring bool) {
	_ = m.accounts.UpdateStatus(accountID, driver.Status{
		State: driver.StateDegraded, Reason: failure.Error(), ObservedAt: time.Now().UTC(),
	})
	if !restoring {
		_, _ = m.accounts.Deactivate(accountID)
	}
}

func (m *Manager) Deactivate(ctx context.Context, value string) (account.Account, error) {
	target, err := m.accounts.Resolve(value)
	if err != nil {
		return account.Account{}, err
	}
	if target.Deleting {
		return account.Account{}, account.ErrDeleting
	}
	operation := m.operations[target.Platform]
	if err := lockOperation(ctx, operation); err != nil {
		return account.Account{}, err
	}
	defer operation.Unlock()
	target, err = m.accounts.Resolve(target.ID)
	if err != nil {
		return account.Account{}, err
	}
	if target.Deleting {
		return account.Account{}, account.ErrDeleting
	}
	m.mu.Lock()
	runtime := m.active[target.Platform]
	if runtime != nil && runtime.account.ID == target.ID {
		if err := runtime.driver.Stop(ctx); err != nil {
			m.mu.Unlock()
			return account.Account{}, fmt.Errorf("stop %s account %s: %w", target.Platform, target.Alias, err)
		}
		runtime.cancel()
		delete(m.active, target.Platform)
	}
	m.mu.Unlock()
	if runtime != nil && runtime.account.ID == target.ID {
		<-runtime.done
	}
	return m.accounts.Deactivate(target.ID)
}

func (m *Manager) Driver(value string) (account.Account, driver.Driver, error) {
	target, err := m.accounts.Resolve(value)
	if err != nil {
		return account.Account{}, nil, err
	}
	if target.Deleting {
		return target, nil, account.ErrDeleting
	}
	m.mu.RLock()
	runtime := m.active[target.Platform]
	m.mu.RUnlock()
	if runtime == nil || runtime.account.ID != target.ID {
		return target, nil, errors.New("account is not active")
	}
	return target, runtime.driver, nil
}

func (m *Manager) Status(ctx context.Context, value string) (driver.Status, error) {
	accountValue, instance, err := m.Driver(value)
	if err != nil {
		if errors.Is(err, account.ErrDeleting) {
			return driver.Status{}, err
		}
		return driver.Status{State: accountValue.State, Identity: accountValue.Identity, Reason: accountValue.LastError, ClientVersion: accountValue.ClientVersion, ObservedAt: time.Now().UTC()}, nil
	}
	status, err := instance.Status(ctx)
	if err == nil {
		_ = m.accounts.UpdateStatus(accountValue.ID, status)
	}
	return status, err
}

func (m *Manager) Shutdown(ctx context.Context) error {
	wechatOperation := m.operations[driver.PlatformWeChat]
	wecomOperation := m.operations[driver.PlatformWeCom]
	if err := lockOperation(ctx, wechatOperation); err != nil {
		return err
	}
	defer wechatOperation.Unlock()
	if err := lockOperation(ctx, wecomOperation); err != nil {
		return err
	}
	defer wecomOperation.Unlock()

	m.mu.Lock()
	m.closed = true
	var stopped []*activeRuntime
	for platform, runtime := range m.active {
		runtime.cancel()
		stopped = append(stopped, runtime)
		delete(m.active, platform)
	}
	m.mu.Unlock()

	// Platform runtimes are independent. Stop them concurrently so their
	// individual container grace periods fit inside the daemon shutdown budget.
	type stopResult struct {
		index int
		err   error
	}
	stopErrors := make([]error, len(stopped))
	results := make(chan stopResult, len(stopped))
	for index, runtime := range stopped {
		go func(index int, runtime *activeRuntime) {
			results <- stopResult{index: index, err: runtime.driver.Stop(ctx)}
		}(index, runtime)
	}
	for range stopped {
		select {
		case result := <-results:
			stopErrors[result.index] = result.err
		case <-ctx.Done():
			return errors.Join(append(stopErrors, ctx.Err())...)
		}
	}
	for _, runtime := range stopped {
		select {
		case <-runtime.done:
		case <-ctx.Done():
			return errors.Join(append(stopErrors, ctx.Err())...)
		}
	}
	return errors.Join(stopErrors...)
}

func (m *Manager) Sync(ctx context.Context, value string) error {
	target, _, err := m.Driver(value)
	if err != nil {
		return err
	}
	m.mu.RLock()
	runtime := m.active[target.Platform]
	m.mu.RUnlock()
	if runtime == nil || runtime.account.ID != target.ID {
		return errors.New("account is not active")
	}
	return m.ingestOnce(ctx, runtime)
}

func lockOperation(ctx context.Context, operation *sync.Mutex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if operation.TryLock() {
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if operation.TryLock() {
				return nil
			}
		}
	}
}

func (m *Manager) ingestLoop(ctx context.Context, runtime *activeRuntime) {
	defer close(runtime.done)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_ = m.ingestOnce(ctx, runtime)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) ingestOnce(ctx context.Context, runtime *activeRuntime) error {
	runtime.ingestMu.Lock()
	defer runtime.ingestMu.Unlock()
	conversations, err := runtime.driver.ListConversations(ctx, driver.ConversationQuery{Limit: 500})
	if err != nil {
		return err
	}
	store, err := m.OpenIndex(runtime.account)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.UpsertConversations(ctx, conversations); err != nil {
		return err
	}
	messages, err := runtime.driver.ReadMessages(ctx, driver.MessageQuery{AfterSequence: runtime.cursor, Limit: 1000})
	if err != nil {
		return err
	}
	var maxCursor = runtime.cursor
	for _, message := range messages {
		if message.Sequence > maxCursor {
			maxCursor = message.Sequence
		}
	}
	if _, err := store.AddMessages(ctx, messages); err != nil {
		return err
	}
	runtime.cursor = maxCursor
	return nil
}

func (m *Manager) OpenIndex(item account.Account) (*index.Store, error) {
	if item.Deleting {
		return nil, account.ErrDeleting
	}
	path := filepath.Join(m.accounts.Runtime(item).StateDir, "index.sqlite3")
	return index.Open(path, item.ID)
}

func (m *Manager) Purge(ctx context.Context, value string) error {
	target, err := m.accounts.Resolve(value)
	if err != nil {
		return err
	}
	if target.Active {
		return errors.New("deactivate the account before purging it")
	}
	operation := m.operations[target.Platform]
	if err := lockOperation(ctx, operation); err != nil {
		return err
	}
	defer operation.Unlock()
	m.mu.RLock()
	factory := m.factories[target.Platform]
	m.mu.RUnlock()
	if factory == nil {
		return fmt.Errorf("no %s driver is installed", target.Platform)
	}
	instance, err := factory(m.accounts.Runtime(target))
	if err != nil {
		return err
	}
	purger, ok := instance.(driver.AccountPurger)
	if !ok {
		return fmt.Errorf("%s driver does not support safe account purge", target.Platform)
	}
	return purger.Purge(ctx, m.accounts.Runtime(target))
}
