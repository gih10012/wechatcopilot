package account

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/gih10012/wechatcopilot/internal/driver"
)

var aliasPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
var accountIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

var (
	ErrDeleting    = errors.New("account deletion is in progress")
	ErrNotDeleting = errors.New("account is not marked for deletion")
)

// IsID reports whether value is an opaque account UUID accepted by the
// persistent registry.
func IsID(value string) bool { return accountIDPattern.MatchString(value) }

type Account struct {
	ID            string              `json:"id"`
	Alias         string              `json:"alias"`
	Platform      driver.Platform     `json:"platform"`
	Active        bool                `json:"active"`
	State         driver.RuntimeState `json:"state"`
	Identity      *driver.Identity    `json:"identity,omitempty"`
	CreatedAt     time.Time           `json:"created_at"`
	UpdatedAt     time.Time           `json:"updated_at"`
	LastError     string              `json:"last_error,omitempty"`
	ClientVersion string              `json:"client_version,omitempty"`
	Deleting      bool                `json:"deleting,omitempty"`
}

type registry struct {
	Version  int       `json:"version"`
	Accounts []Account `json:"accounts"`
}

type Store struct {
	mu           sync.RWMutex
	paths        config.Paths
	data         registry
	writeFile    func(string, []byte, fs.FileMode) error
	newAccountID func() string
}

func Open(paths config.Paths) (*Store, error) {
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	store := &Store{paths: paths, data: registry{Version: 1}, writeFile: config.AtomicWrite, newAccountID: newID}
	info, statErr := os.Lstat(paths.Registry)
	if statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("account registry must be a regular, non-symlink file")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("account registry is accessible by other users")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	contents, err := os.ReadFile(paths.Registry)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(contents, &store.data); err != nil {
		return nil, fmt.Errorf("decode account registry: %w", err)
	}
	if store.data.Version != 1 {
		return nil, fmt.Errorf("unsupported account registry version %d", store.data.Version)
	}
	normalized := false
	ids := make(map[string]struct{}, len(store.data.Accounts))
	aliases := make(map[string]struct{}, len(store.data.Accounts))
	for i := range store.data.Accounts {
		item := &store.data.Accounts[i]
		if !accountIDPattern.MatchString(item.ID) {
			return nil, fmt.Errorf("account registry contains invalid account ID %q", item.ID)
		}
		if !aliasPattern.MatchString(item.Alias) {
			return nil, fmt.Errorf("account registry contains invalid alias %q", item.Alias)
		}
		if item.Platform != driver.PlatformWeChat && item.Platform != driver.PlatformWeCom {
			return nil, fmt.Errorf("account registry contains invalid platform %q", item.Platform)
		}
		if item.ID == item.Alias {
			return nil, fmt.Errorf("account registry ID %q cannot also be an alias", item.ID)
		}
		if _, exists := ids[item.ID]; exists {
			return nil, fmt.Errorf("account registry contains duplicate account ID %q", item.ID)
		}
		if _, exists := aliases[item.Alias]; exists {
			return nil, fmt.Errorf("account registry contains duplicate alias %q", item.Alias)
		}
		if _, exists := aliases[item.ID]; exists {
			return nil, fmt.Errorf("account registry ID %q conflicts with another account alias", item.ID)
		}
		if _, exists := ids[item.Alias]; exists {
			return nil, fmt.Errorf("account registry alias %q conflicts with another account ID", item.Alias)
		}
		ids[item.ID] = struct{}{}
		aliases[item.Alias] = struct{}{}
		if item.Deleting && (item.Active || item.State != driver.StateStopped) {
			item.Active = false
			item.State = driver.StateStopped
			normalized = true
		}
	}
	if normalized {
		if err := store.saveLocked(); err != nil {
			return nil, fmt.Errorf("normalize deleting accounts: %w", err)
		}
	}
	return store, nil
}

func (s *Store) Add(alias string, platform driver.Platform) (Account, error) {
	if !aliasPattern.MatchString(alias) {
		return Account{}, fmt.Errorf("alias must match %s", aliasPattern.String())
	}
	if platform != driver.PlatformWeChat && platform != driver.PlatformWeCom {
		return Account{}, fmt.Errorf("unsupported platform %q", platform)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.data.Accounts {
		if existing.Alias == alias {
			return Account{}, fmt.Errorf("account alias %q already exists", alias)
		}
		if existing.ID == alias {
			return Account{}, fmt.Errorf("account alias %q conflicts with an existing account ID", alias)
		}
	}
	id, err := s.nextAccountIDLocked(alias)
	if err != nil {
		return Account{}, err
	}
	now := time.Now().UTC()
	account := Account{ID: id, Alias: alias, Platform: platform, State: driver.StateStopped, CreatedAt: now, UpdatedAt: now}
	accountDir := filepath.Join(s.paths.Accounts, account.ID)
	if err := os.Mkdir(accountDir, 0o700); err != nil {
		return Account{}, err
	}
	if err := os.Mkdir(filepath.Join(accountDir, "profile"), 0o700); err != nil {
		return Account{}, errors.Join(err, os.RemoveAll(accountDir))
	}
	next := cloneRegistry(s.data)
	next.Accounts = append(next.Accounts, account)
	if err := s.saveDataLocked(next); err != nil {
		return Account{}, errors.Join(err, os.RemoveAll(accountDir))
	}
	s.data = next
	return account, nil
}

func (s *Store) nextAccountIDLocked(newAlias string) (string, error) {
	generate := s.newAccountID
	if generate == nil {
		generate = newID
	}
	for attempt := 0; attempt < 128; attempt++ {
		id := generate()
		if !accountIDPattern.MatchString(id) || id == newAlias {
			continue
		}
		available := true
		for _, existing := range s.data.Accounts {
			if existing.ID == id || existing.Alias == id {
				available = false
				break
			}
		}
		if !available {
			continue
		}
		_, err := os.Lstat(filepath.Join(s.paths.Accounts, id))
		if errors.Is(err, os.ErrNotExist) {
			return id, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate a unique account ID")
}

func (s *Store) List() []Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := cloneAccounts(s.data.Accounts)
	sort.Slice(result, func(i, j int) bool {
		if result[i].Platform == result[j].Platform {
			return result[i].Alias < result[j].Alias
		}
		return result[i].Platform < result[j].Platform
	})
	return result
}

func (s *Store) Resolve(value string) (Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resolveLocked(value)
}

func (s *Store) resolveLocked(value string) (Account, error) {
	for _, account := range s.data.Accounts {
		if account.ID == value || account.Alias == value {
			return cloneAccount(account), nil
		}
	}
	return Account{}, os.ErrNotExist
}

func (s *Store) Activate(value string) (Account, *Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, err := s.resolveLocked(value)
	if err != nil {
		return Account{}, nil, err
	}
	if target.Deleting {
		return Account{}, nil, ErrDeleting
	}
	next := cloneRegistry(s.data)
	now := time.Now().UTC()
	var previous *Account
	for i := range next.Accounts {
		if next.Accounts[i].Platform == target.Platform && next.Accounts[i].Active && next.Accounts[i].ID != target.ID {
			previousAccount := cloneAccount(next.Accounts[i])
			previous = &previousAccount
			next.Accounts[i].Active = false
			next.Accounts[i].State = driver.StateStopped
			next.Accounts[i].UpdatedAt = now
		}
		if next.Accounts[i].ID == target.ID {
			next.Accounts[i].Active = true
			next.Accounts[i].State = driver.StateStarting
			next.Accounts[i].UpdatedAt = now
			target = cloneAccount(next.Accounts[i])
		}
	}
	if err := s.saveDataLocked(next); err != nil {
		return Account{}, nil, err
	}
	s.data = next
	return target, previous, nil
}

func (s *Store) Deactivate(value string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, err := s.resolveLocked(value)
	if err != nil {
		return Account{}, err
	}
	if account.Deleting {
		return Account{}, ErrDeleting
	}
	next := cloneRegistry(s.data)
	for i := range next.Accounts {
		if next.Accounts[i].ID == account.ID {
			next.Accounts[i].Active = false
			next.Accounts[i].State = driver.StateStopped
			next.Accounts[i].UpdatedAt = time.Now().UTC()
			account = cloneAccount(next.Accounts[i])
		}
	}
	if err := s.saveDataLocked(next); err != nil {
		return Account{}, err
	}
	s.data = next
	return account, nil
}

func (s *Store) UpdateStatus(id string, status driver.Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneRegistry(s.data)
	for i := range next.Accounts {
		if next.Accounts[i].ID == id {
			if next.Accounts[i].Deleting {
				return ErrDeleting
			}
			next.Accounts[i].State = status.State
			next.Accounts[i].Identity = cloneIdentity(status.Identity)
			next.Accounts[i].LastError = status.Reason
			next.Accounts[i].ClientVersion = status.ClientVersion
			next.Accounts[i].UpdatedAt = time.Now().UTC()
			if err := s.saveDataLocked(next); err != nil {
				return err
			}
			s.data = next
			return nil
		}
	}
	return os.ErrNotExist
}

func (s *Store) Runtime(account Account) driver.AccountRuntime {
	accountDir := filepath.Join(s.paths.Accounts, account.ID)
	return driver.AccountRuntime{
		AccountID:  account.ID,
		Alias:      account.Alias,
		StateDir:   accountDir,
		RuntimeDir: filepath.Join(s.paths.Runtime, "accounts", account.ID),
	}
}

// BeginDelete durably makes an inactive account unusable before external
// runtime and profile cleanup begins. Repeating it is safe after a crash.
func (s *Store) BeginDelete(value string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, err := s.resolveLocked(value)
	if err != nil {
		return Account{}, err
	}
	if account.Active {
		return Account{}, errors.New("deactivate the account before removing it")
	}
	if account.Deleting {
		return account, nil
	}
	next := cloneRegistry(s.data)
	for i := range next.Accounts {
		if next.Accounts[i].ID != account.ID {
			continue
		}
		next.Accounts[i].Deleting = true
		next.Accounts[i].Active = false
		next.Accounts[i].State = driver.StateStopped
		next.Accounts[i].LastError = ""
		next.Accounts[i].UpdatedAt = time.Now().UTC()
		if err := s.saveDataLocked(next); err != nil {
			return Account{}, err
		}
		s.data = next
		return cloneAccount(next.Accounts[i]), nil
	}
	return Account{}, os.ErrNotExist
}

// RecordDeleteFailure keeps a retryable deletion visible without allowing the
// account to return to service.
func (s *Store) RecordDeleteFailure(id string, failure error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if failure == nil {
		return errors.New("account deletion failure is required")
	}
	next := cloneRegistry(s.data)
	for i := range next.Accounts {
		if next.Accounts[i].ID != id {
			continue
		}
		if !next.Accounts[i].Deleting {
			return ErrNotDeleting
		}
		next.Accounts[i].LastError = failure.Error()
		next.Accounts[i].UpdatedAt = time.Now().UTC()
		if err := s.saveDataLocked(next); err != nil {
			return err
		}
		s.data = next
		return nil
	}
	return os.ErrNotExist
}

// FinalizeDelete removes local account state before atomically removing the
// durable deletion marker. A failure leaves the marker available for retry.
func (s *Store) FinalizeDelete(value string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, err := s.resolveLocked(value)
	if err != nil {
		return Account{}, err
	}
	if !account.Deleting {
		return Account{}, ErrNotDeleting
	}
	stateDir, err := deletionTarget(s.paths.Accounts, account.ID)
	if err != nil {
		return Account{}, err
	}
	runtimeDir, err := deletionTarget(filepath.Join(s.paths.Runtime, "accounts"), account.ID)
	if err != nil {
		return Account{}, err
	}
	if err := os.RemoveAll(runtimeDir); err != nil {
		return Account{}, fmt.Errorf("remove account runtime state: %w", err)
	}
	if err := os.RemoveAll(stateDir); err != nil {
		return Account{}, fmt.Errorf("remove account persistent state: %w", err)
	}

	next := registry{Version: s.data.Version, Accounts: make([]Account, 0, len(s.data.Accounts)-1)}
	for _, item := range s.data.Accounts {
		if item.ID != account.ID {
			next.Accounts = append(next.Accounts, cloneAccount(item))
		}
	}
	if err := s.saveDataLocked(next); err != nil {
		return Account{}, err
	}
	s.data = next
	return account, nil
}

func deletionTarget(root, id string) (string, error) {
	if !accountIDPattern.MatchString(id) {
		return "", fmt.Errorf("refusing to delete invalid account ID %q", id)
	}
	root = filepath.Clean(root)
	target := filepath.Join(root, id)
	relative, err := filepath.Rel(root, target)
	if err != nil || relative != id {
		return "", errors.New("account deletion target escapes its managed root")
	}
	return target, nil
}

func (s *Store) saveLocked() error {
	return s.saveDataLocked(s.data)
}

func (s *Store) saveDataLocked(data registry) error {
	contents, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	writer := s.writeFile
	if writer == nil {
		writer = config.AtomicWrite
	}
	return writer(s.paths.Registry, contents, 0o600)
}

func cloneRegistry(value registry) registry {
	return registry{Version: value.Version, Accounts: cloneAccounts(value.Accounts)}
}

func cloneAccounts(values []Account) []Account {
	result := make([]Account, len(values))
	for i := range values {
		result[i] = cloneAccount(values[i])
	}
	return result
}

func cloneAccount(value Account) Account {
	value.Identity = cloneIdentity(value.Identity)
	return value
}

func cloneIdentity(value *driver.Identity) *driver.Identity {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func newID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
