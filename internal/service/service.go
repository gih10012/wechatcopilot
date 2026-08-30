package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gih10012/wechatcopilot/internal/account"
	"github.com/gih10012/wechatcopilot/internal/api"
	"github.com/gih10012/wechatcopilot/internal/auth"
	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/gih10012/wechatcopilot/internal/driver"
	runtimemgr "github.com/gih10012/wechatcopilot/internal/runtime"
)

const transactionTTL = 5 * time.Minute

type PreparedSend struct {
	ID                string               `json:"id"`
	AccountID         string               `json:"account_id"`
	AccountAlias      string               `json:"account_alias"`
	ConversationID    string               `json:"conversation_id"`
	ConversationTitle string               `json:"conversation_title"`
	Text              string               `json:"text,omitempty"`
	Attachments       []PreparedAttachment `json:"attachments,omitempty"`
	ShareSurfaceID    string               `json:"share_surface_id,omitempty"`
	Warnings          []string             `json:"warnings,omitempty"`
	RequestHash       string               `json:"request_hash"`
	ExpiresAt         time.Time            `json:"expires_at"`
}

type PreparedAttachment struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Service struct {
	paths           config.Paths
	accounts        *account.Store
	runtimes        *runtimemgr.Manager
	auth            *auth.Manager
	sendMu          sync.Mutex
	deleteMu        sync.RWMutex
	transactionsMu  sync.Mutex
	transactions    map[string]preparedTransaction
	transactionStop chan struct{}
	transactionDone chan struct{}
	transactionOnce sync.Once
}

func New(paths config.Paths, factories map[driver.Platform]driver.Factory) (*Service, error) {
	accounts, err := account.Open(paths)
	if err != nil {
		return nil, err
	}
	runtimes := runtimemgr.NewManager(accounts)
	for platform, factory := range factories {
		runtimes.Register(platform, factory)
	}
	service := &Service{
		paths: paths, accounts: accounts, runtimes: runtimes, auth: auth.NewManager(paths),
		transactions: make(map[string]preparedTransaction), transactionStop: make(chan struct{}),
		transactionDone: make(chan struct{}),
	}
	if err := service.initializeTransactionStaging(); err != nil {
		return nil, err
	}
	go service.runTransactionReaper()
	return service, nil
}

func (s *Service) Restore(ctx context.Context) []error { return s.runtimes.Restore(ctx) }

func (s *Service) Close(ctx context.Context) error {
	s.auth.CloseAll()
	s.sendMu.Lock()
	stagingErr := s.closeTransactions()
	s.sendMu.Unlock()
	return errors.Join(stagingErr, s.runtimes.Shutdown(ctx))
}

func (s *Service) Accounts() []account.Account { return s.accounts.List() }

func (s *Service) AddAccount(alias string, platform driver.Platform) (account.Account, error) {
	item, err := s.accounts.Add(alias, platform)
	if err != nil {
		return account.Account{}, api.WrapError(http.StatusBadRequest, api.CodeInvalidArgument, "cannot add account", err)
	}
	return item, nil
}

func (s *Service) Account(value string) (account.Account, error) {
	item, err := s.accounts.Resolve(value)
	if errors.Is(err, os.ErrNotExist) {
		return account.Account{}, api.NewError(http.StatusNotFound, api.CodeNotFound, "account not found")
	}
	if err != nil {
		return account.Account{}, err
	}
	if item.Deleting {
		return account.Account{}, deletingAccountError(item, "account deletion is in progress", account.ErrDeleting)
	}
	return item, nil
}

func (s *Service) Activate(ctx context.Context, value string) (account.Account, error) {
	item, err := s.runtimes.Activate(ctx, value)
	if err != nil {
		if errors.Is(err, account.ErrDeleting) {
			deleting, resolveErr := s.accounts.Resolve(value)
			if resolveErr == nil {
				return account.Account{}, deletingAccountError(deleting, "account deletion is in progress", err)
			}
		}
		code := api.CodeDriverUnavailable
		status := http.StatusServiceUnavailable
		if errors.Is(err, os.ErrNotExist) {
			code = api.CodeNotFound
			status = http.StatusNotFound
		}
		if classified := classifiedDriverError(err); classified != nil {
			return account.Account{}, classified
		}
		return account.Account{}, api.WrapError(status, code, "cannot activate account", err)
	}
	return item, nil
}

func (s *Service) Deactivate(ctx context.Context, value string) (account.Account, error) {
	item, err := s.runtimes.Deactivate(ctx, value)
	if err != nil {
		if errors.Is(err, account.ErrDeleting) {
			deleting, resolveErr := s.accounts.Resolve(value)
			if resolveErr == nil {
				return account.Account{}, deletingAccountError(deleting, "account deletion is in progress", err)
			}
		}
		if classified := classifiedDriverError(err); classified != nil {
			return account.Account{}, classified
		}
		return account.Account{}, api.WrapError(http.StatusBadRequest, api.CodeInvalidArgument, "cannot deactivate account", err)
	}
	return item, nil
}

func (s *Service) RemoveAccount(ctx context.Context, value string, purge, confirmed bool) (account.Account, error) {
	if !confirmed {
		return account.Account{}, api.NewError(http.StatusConflict, api.CodeConfirmationRequired, "account removal requires confirmed=true")
	}
	if !purge {
		return account.Account{}, api.NewError(http.StatusBadRequest, api.CodeInvalidArgument, "account removal requires purge=true in v0.1")
	}
	if !account.IsID(value) {
		return account.Account{}, api.NewError(http.StatusBadRequest, api.CodeInvalidArgument, "account removal requires an exact opaque account ID")
	}
	s.deleteMu.Lock()
	defer s.deleteMu.Unlock()

	item, err := s.runtimes.BeginDelete(value)
	if errors.Is(err, os.ErrNotExist) {
		return account.Account{}, api.NewError(http.StatusNotFound, api.CodeNotFound, "account not found")
	}
	if err != nil {
		return account.Account{}, api.WrapError(http.StatusConflict, api.CodeConflict, "cannot begin account deletion", err)
	}
	if err := s.runtimes.Purge(ctx, item.ID); err != nil {
		return account.Account{}, s.recordDeleteFailure(item, "account runtime purge is incomplete; retry the same removal request", err)
	}
	removed, err := s.accounts.FinalizeDelete(item.ID)
	if err != nil {
		return account.Account{}, s.recordDeleteFailure(item, "account state deletion is incomplete; retry the same removal request", err)
	}
	return removed, nil
}

func (s *Service) recordDeleteFailure(item account.Account, message string, failure error) error {
	if err := s.accounts.RecordDeleteFailure(item.ID, failure); err != nil {
		failure = errors.Join(failure, fmt.Errorf("persist deletion failure: %w", err))
	}
	return deletingAccountError(item, message, failure)
}

func deletingAccountError(item account.Account, message string, cause error) *api.AppError {
	err := api.WrapError(http.StatusConflict, api.CodeConflict, message, cause)
	err.Details = map[string]any{
		"account_id": item.ID,
		"deleting":   true,
		"retryable":  true,
	}
	return err
}

func (s *Service) Status(ctx context.Context, value string) (driver.Status, error) {
	if _, err := s.Account(value); err != nil {
		return driver.Status{}, err
	}
	status, err := s.runtimes.Status(ctx, value)
	if err != nil {
		if classified := classifiedDriverError(err); classified != nil {
			return driver.Status{}, classified
		}
		return driver.Status{}, api.WrapError(http.StatusServiceUnavailable, api.CodeDriverUnavailable, "cannot read driver status", err)
	}
	return status, nil
}

func (s *Service) BeginAuth(ctx context.Context, value string, lan bool, lanAddress string) (auth.Challenge, error) {
	resolvedLANAddress, err := auth.ResolveLANAddress(lan, lanAddress)
	if errors.Is(err, auth.ErrInvalidLANAddress) {
		return auth.Challenge{}, api.WrapError(http.StatusBadRequest, api.CodeInvalidArgument, "invalid LAN login address", err)
	}
	if err != nil {
		return auth.Challenge{}, api.WrapError(http.StatusInternalServerError, api.CodeInternal, "cannot inspect LAN interfaces", err)
	}
	item, instance, err := s.runtimes.Driver(value)
	if err != nil {
		if _, activateErr := s.Activate(ctx, value); activateErr != nil {
			return auth.Challenge{}, activateErr
		}
		item, instance, err = s.runtimes.Driver(value)
	}
	if err != nil {
		return auth.Challenge{}, api.WrapError(http.StatusServiceUnavailable, api.CodeDriverUnavailable, "account driver is unavailable", err)
	}
	challenge, err := s.auth.Begin(ctx, item.ID, instance, lan, resolvedLANAddress)
	if errors.Is(err, auth.ErrInvalidLANAddress) {
		return auth.Challenge{}, api.WrapError(http.StatusBadRequest, api.CodeInvalidArgument, "invalid LAN login address", err)
	}
	if err != nil {
		return auth.Challenge{}, api.WrapError(http.StatusInternalServerError, api.CodeInternal, "cannot start authentication challenge", err)
	}
	return challenge, nil
}

func (s *Service) AuthStatus(id string) (auth.Challenge, error) {
	challenge, err := s.auth.Status(id)
	if errors.Is(err, os.ErrNotExist) {
		return auth.Challenge{}, api.NewError(http.StatusNotFound, api.CodeAuthExpired, "authentication challenge not found or expired")
	}
	return challenge, err
}

func (s *Service) SubmitAuthCode(ctx context.Context, id, code string) error {
	err := s.auth.SubmitCode(ctx, id, code)
	if errors.Is(err, os.ErrNotExist) {
		return api.NewError(http.StatusNotFound, api.CodeAuthExpired, "authentication challenge not found or expired")
	}
	if err != nil {
		if classified := classifiedDriverError(err); classified != nil {
			return classified
		}
		return api.WrapError(http.StatusBadRequest, api.CodeInvalidArgument, "verification code was rejected", err)
	}
	return nil
}

func (s *Service) Capabilities(ctx context.Context, value string) (map[string]driver.Support, error) {
	status, err := s.Status(ctx, value)
	if err != nil {
		return nil, err
	}
	if status.Capabilities == nil {
		status.Capabilities = map[string]driver.Support{}
	}
	return status.Capabilities, nil
}

func (s *Service) ListConversations(ctx context.Context, value, search string, unread bool, limit int) ([]driver.Conversation, error) {
	s.deleteMu.RLock()
	defer s.deleteMu.RUnlock()
	item, err := s.Account(value)
	if err != nil {
		return nil, err
	}
	if item.Active {
		_ = s.runtimes.Sync(ctx, item.ID)
	}
	store, err := s.runtimes.OpenIndex(item)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.ListConversations(ctx, search, unread, limit)
}

func (s *Service) ListMessages(ctx context.Context, value string, query driver.MessageQuery) ([]driver.Message, error) {
	if query.Latest && query.AfterSequence != 0 {
		return nil, api.NewError(http.StatusBadRequest, api.CodeInvalidArgument, "latest message reads cannot use a nonzero sequence cursor")
	}
	s.deleteMu.RLock()
	defer s.deleteMu.RUnlock()
	item, err := s.Account(value)
	if err != nil {
		return nil, err
	}
	if item.Active {
		_ = s.runtimes.Sync(ctx, item.ID)
	}
	store, err := s.runtimes.OpenIndex(item)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.ListMessages(ctx, query)
}

func (s *Service) SearchMessages(ctx context.Context, value, text string, limit int) ([]driver.Message, error) {
	s.deleteMu.RLock()
	defer s.deleteMu.RUnlock()
	item, err := s.Account(value)
	if err != nil {
		return nil, err
	}
	store, err := s.runtimes.OpenIndex(item)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.SearchMessages(ctx, text, limit)
}

func (s *Service) PollMessages(ctx context.Context, value string, after int64, timeout time.Duration, limit int) ([]driver.Message, error) {
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 25 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		messages, err := s.ListMessages(ctx, value, driver.MessageQuery{AfterSequence: after, Limit: limit})
		if err != nil {
			return nil, err
		}
		if len(messages) > 0 {
			return messages, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return []driver.Message{}, nil
		case <-ticker.C:
		}
	}
}

func (s *Service) PrepareSend(ctx context.Context, value string, request driver.SendRequest) (PreparedSend, error) {
	item, _, err := s.runtimes.Driver(value)
	if err != nil {
		return PreparedSend{}, api.NewError(http.StatusConflict, api.CodeAccountInactive, "account must be active before sending")
	}
	if strings.TrimSpace(request.Text) == "" && len(request.Attachments) == 0 && request.ShareSurfaceID == "" {
		return PreparedSend{}, api.NewError(http.StatusBadRequest, api.CodeInvalidArgument, "message content is empty")
	}
	conversations, err := s.ListConversations(ctx, item.ID, "", false, 500)
	if err != nil {
		return PreparedSend{}, err
	}
	var matches []driver.Conversation
	for _, conversation := range conversations {
		if conversation.ID == request.ConversationID {
			matches = append(matches, conversation)
		}
	}
	if len(matches) == 0 {
		return PreparedSend{}, api.NewError(http.StatusNotFound, api.CodeNotFound, "conversation not found")
	}
	if len(matches) > 1 {
		return PreparedSend{}, api.NewError(http.StatusConflict, api.CodeTargetAmbiguous, "conversation target is ambiguous")
	}
	transactionID := randomID()
	previewAttachments, stagedAttachments, stageDir, err := s.stageSendAttachments(transactionID, request.Attachments)
	if err != nil {
		return PreparedSend{}, api.WrapError(http.StatusBadRequest, api.CodeInvalidArgument, "cannot stage attachments", err)
	}
	requestHash := canonicalSendHash(request.ConversationID, request.Text, previewAttachments, request.ShareSurfaceID)
	var warnings []string
	if matches[0].Kind == "group" {
		warnings = append(warnings, "this message targets a group conversation")
	}
	if len(previewAttachments) > 0 {
		warnings = append(warnings, "attachments were copied into immutable staging; edits to the source files will not change this send")
	}
	if request.ShareSurfaceID != "" {
		warnings = append(warnings, "this send shares the currently prepared webpage or mini-program surface")
	}
	prepared := PreparedSend{
		ID: transactionID, AccountID: item.ID, AccountAlias: item.Alias,
		ConversationID: request.ConversationID, ConversationTitle: matches[0].Title,
		Text: request.Text, Attachments: previewAttachments, ShareSurfaceID: request.ShareSurfaceID,
		Warnings: warnings, RequestHash: requestHash, ExpiresAt: time.Now().UTC().Add(transactionTTL),
	}
	s.transactionsMu.Lock()
	s.pruneTransactionsLocked()
	s.transactions[prepared.ID] = preparedTransaction{
		Preview: prepared, Attachments: stagedAttachments, StageDir: stageDir,
	}
	s.transactionsMu.Unlock()
	return prepared, nil
}

func (s *Service) CommitSend(ctx context.Context, transactionID, idempotencyKey string, confirmed bool) (driver.SendResult, error) {
	if !confirmed {
		return driver.SendResult{}, api.NewError(http.StatusConflict, api.CodeConfirmationRequired, "send transaction requires confirmed=true")
	}
	if strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 128 {
		return driver.SendResult{}, api.NewError(http.StatusBadRequest, api.CodeInvalidArgument, "a bounded idempotency key is required")
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.deleteMu.RLock()
	defer s.deleteMu.RUnlock()
	s.transactionsMu.Lock()
	transaction, ok := s.transactions[transactionID]
	if ok && !time.Now().UTC().Before(transaction.Preview.ExpiresAt) {
		_ = s.cleanupTransactionLocked(transactionID)
		ok = false
	}
	if ok {
		transaction.Committing = true
		s.transactions[transactionID] = transaction
	}
	s.transactionsMu.Unlock()
	if !ok {
		if result, found, err := s.findCommittedSend(ctx, transactionID, idempotencyKey); err != nil {
			return driver.SendResult{}, err
		} else if found {
			return sendOutcome(result)
		}
		return driver.SendResult{}, api.NewError(http.StatusNotFound, api.CodeNotFound, "send transaction not found or expired")
	}
	defer s.releaseTransactionCommit(transactionID)
	prepared := transaction.Preview
	item, err := s.Account(prepared.AccountID)
	if err != nil {
		return driver.SendResult{}, err
	}
	store, err := s.runtimes.OpenIndex(item)
	if err != nil {
		return driver.SendResult{}, err
	}
	defer store.Close()
	if storedKey, existing, err := store.GetSendResultForTransaction(ctx, transactionID); err != nil {
		return driver.SendResult{}, err
	} else if existing != nil {
		s.cleanupTransaction(transactionID)
		if storedKey != idempotencyKey {
			return driver.SendResult{}, api.NewError(http.StatusConflict, api.CodeConflict, "send transaction was already committed with another idempotency key")
		}
		return sendOutcome(*existing)
	}
	if existing, err := store.GetSendResult(ctx, idempotencyKey, prepared.RequestHash); err != nil {
		return driver.SendResult{}, api.WrapError(http.StatusConflict, api.CodeConflict, "idempotency key conflict", err)
	} else if existing != nil {
		s.cleanupTransaction(transactionID)
		return sendOutcome(*existing)
	}
	_, instance, err := s.runtimes.Driver(prepared.AccountID)
	if err != nil {
		return driver.SendResult{}, api.NewError(http.StatusConflict, api.CodeAccountInactive, "prepared account is no longer active")
	}
	provisional := driver.SendResult{
		Uncertain: true,
		Detail:    "send attempt was durably reserved, but no verified completion was recorded; it will not be retried",
	}
	if err := store.ReserveSend(ctx, idempotencyKey, transactionID, prepared.RequestHash, provisional); err != nil {
		return driver.SendResult{}, api.WrapError(http.StatusConflict, api.CodeConflict, "cannot reserve idempotent send", err)
	}
	result, sendErr := instance.Send(ctx, driver.SendRequest{
		ConversationID: prepared.ConversationID, Text: prepared.Text, Attachments: transaction.Attachments,
		ShareSurfaceID: prepared.ShareSurfaceID, IdempotencyKey: idempotencyKey,
	})
	if sendErr != nil && !result.Uncertain {
		if err := store.DeleteSendReservation(ctx, idempotencyKey, transactionID, prepared.RequestHash); err != nil {
			s.cleanupTransaction(transactionID)
			return sendOutcome(provisional)
		}
		if classified := classifiedDriverError(sendErr); classified != nil {
			return driver.SendResult{}, classified
		}
		return driver.SendResult{}, api.WrapError(http.StatusBadGateway, api.CodePartialFailure, "client send failed before any send action", sendErr)
	}
	if !result.Verified {
		result.Uncertain = true
		if result.Detail == "" {
			result.Detail = "client could not verify whether the message was sent"
		}
	}
	if err := store.FinalizeSend(ctx, idempotencyKey, transactionID, prepared.RequestHash, result); err != nil {
		s.cleanupTransaction(transactionID)
		return sendOutcome(provisional)
	}
	s.cleanupTransaction(transactionID)
	return sendOutcome(result)
}

func (s *Service) findCommittedSend(ctx context.Context, transactionID, idempotencyKey string) (driver.SendResult, bool, error) {
	for _, item := range s.accounts.List() {
		store, err := s.runtimes.OpenIndex(item)
		if err != nil {
			return driver.SendResult{}, false, err
		}
		storedKey, result, lookupErr := store.GetSendResultForTransaction(ctx, transactionID)
		closeErr := store.Close()
		if lookupErr != nil {
			return driver.SendResult{}, false, lookupErr
		}
		if closeErr != nil {
			return driver.SendResult{}, false, closeErr
		}
		if result != nil {
			if storedKey != idempotencyKey {
				return driver.SendResult{}, false, api.NewError(http.StatusConflict, api.CodeConflict, "send transaction was already committed with another idempotency key")
			}
			return *result, true, nil
		}
	}
	return driver.SendResult{}, false, nil
}

func sendOutcome(result driver.SendResult) (driver.SendResult, error) {
	if result.Uncertain || !result.Verified {
		appErr := api.NewError(http.StatusConflict, api.CodeSendUncertain, "client could not verify the sent message")
		appErr.Details = map[string]any{"result": result}
		return result, appErr
	}
	return result, nil
}

func (s *Service) OpenSurface(ctx context.Context, value, ref string) (driver.Surface, error) {
	_, instance, err := s.runtimes.Driver(value)
	if err != nil {
		return driver.Surface{}, api.NewError(http.StatusConflict, api.CodeAccountInactive, "account must be active")
	}
	surface, err := instance.OpenSurface(ctx, ref)
	if classified := classifiedDriverError(err); classified != nil {
		return driver.Surface{}, classified
	}
	return surface, err
}

func (s *Service) OpenNamedSurface(ctx context.Context, value string, target driver.NamedSurface) (driver.Surface, error) {
	_, instance, err := s.runtimes.Driver(value)
	if err != nil {
		return driver.Surface{}, api.NewError(http.StatusConflict, api.CodeAccountInactive, "account must be active")
	}
	opener, ok := instance.(driver.NamedSurfaceOpener)
	if !ok {
		return driver.Surface{}, api.NewError(http.StatusConflict, api.CodeUnsupportedCapability, "the active driver cannot open mini programs by name")
	}
	surface, err := opener.OpenNamedSurface(ctx, target)
	if classified := classifiedDriverError(err); classified != nil {
		return driver.Surface{}, classified
	}
	return surface, err
}

func (s *Service) SnapshotSurface(ctx context.Context, value, id string) (driver.Surface, error) {
	_, instance, err := s.runtimes.Driver(value)
	if err != nil {
		return driver.Surface{}, api.NewError(http.StatusConflict, api.CodeAccountInactive, "account must be active")
	}
	surface, err := instance.SnapshotSurface(ctx, id)
	if classified := classifiedDriverError(err); classified != nil {
		return driver.Surface{}, classified
	}
	return surface, err
}

func (s *Service) ActSurface(ctx context.Context, value, id string, action driver.SurfaceAction) (driver.Surface, error) {
	_, instance, err := s.runtimes.Driver(value)
	if err != nil {
		return driver.Surface{}, api.NewError(http.StatusConflict, api.CodeAccountInactive, "account must be active")
	}
	forbidden := map[string]bool{"pay": true, "transfer": true, "red-packet": true, "grant-permission": true}
	if forbidden[action.ActionID] {
		return driver.Surface{}, api.NewError(http.StatusConflict, api.CodeUserActionRequired, "this action requires direct user interaction")
	}
	surface, err := instance.ActSurface(ctx, id, action)
	if classified := classifiedDriverError(err); classified != nil {
		return driver.Surface{}, classified
	}
	return surface, err
}

func (s *Service) CloseSurface(ctx context.Context, value, id string) error {
	_, instance, err := s.runtimes.Driver(value)
	if err != nil {
		return api.NewError(http.StatusConflict, api.CodeAccountInactive, "account must be active")
	}
	err = instance.CloseSurface(ctx, id)
	if classified := classifiedDriverError(err); classified != nil {
		return classified
	}
	return err
}

func (s *Service) ExportSurfaceAsset(ctx context.Context, value, id, token string) (driver.SurfaceAssetExport, error) {
	_, instance, err := s.runtimes.Driver(value)
	if err != nil {
		return driver.SurfaceAssetExport{}, api.NewError(http.StatusConflict, api.CodeAccountInactive, "account must be active")
	}
	exporter, ok := instance.(driver.SurfaceAssetExporter)
	if !ok {
		return driver.SurfaceAssetExport{}, api.NewError(http.StatusConflict, api.CodeUnsupportedCapability, "the active driver cannot export surface assets")
	}
	result, err := exporter.ExportSurfaceAsset(ctx, id, token)
	if classified := classifiedDriverError(err); classified != nil {
		return driver.SurfaceAssetExport{}, classified
	}
	return result, err
}

func (s *Service) ActiveAccounts() []account.Account {
	var result []account.Account
	for _, item := range s.accounts.List() {
		if item.Active {
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Platform < result[j].Platform })
	return result
}

func randomID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(fmt.Sprintf("secure random failed: %v", err))
	}
	return hex.EncodeToString(value[:])
}
