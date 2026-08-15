package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gih10012/wechatcopilot/internal/api"
	"github.com/gih10012/wechatcopilot/internal/config"
	"github.com/gih10012/wechatcopilot/internal/driver"
	"github.com/gih10012/wechatcopilot/internal/driver/fake"
)

func TestMultiAccountAndTransactionalSend(t *testing.T) {
	ctx := context.Background()
	paths := testPaths(t)
	service, err := New(paths, fakeFactories())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })

	wechatOne, err := service.AddAccount("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	wechatTwo, err := service.AddAccount("backup", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	wecomAccount, err := service.AddAccount("work", driver.PlatformWeCom)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Activate(ctx, wechatOne.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Activate(ctx, wechatTwo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Activate(ctx, wecomAccount.ID); err != nil {
		t.Fatal(err)
	}

	first, err := service.Account(wechatOne.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Account(wechatTwo.ID)
	if err != nil {
		t.Fatal(err)
	}
	work, err := service.Account(wecomAccount.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Active || !second.Active || !work.Active {
		t.Fatalf("unexpected active accounts: first=%v second=%v work=%v", first.Active, second.Active, work.Active)
	}

	conversations, err := service.ListConversations(ctx, second.ID, "", false, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 1 || conversations[0].ID != "conv-file-transfer" {
		t.Fatalf("unexpected conversations: %#v", conversations)
	}
	prepared, err := service.PrepareSend(ctx, second.ID, driver.SendRequest{
		ConversationID: conversations[0].ID,
		Text:           "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CommitSend(ctx, prepared.ID, "send-1", false); appErrorCode(err) != api.CodeConfirmationRequired {
		t.Fatalf("commit without confirmation: %v", err)
	}
	result, err := service.CommitSend(ctx, prepared.ID, "send-1", true)
	if err != nil || !result.Verified {
		t.Fatalf("commit: result=%#v err=%v", result, err)
	}
	replayed, err := service.CommitSend(ctx, prepared.ID, "send-1", true)
	if err != nil || replayed.MessageID != result.MessageID {
		t.Fatalf("idempotent replay: result=%#v err=%v", replayed, err)
	}
	if _, err := service.CommitSend(ctx, prepared.ID, "different-key", true); appErrorCode(err) != api.CodeConflict {
		t.Fatalf("transaction reused with another key: %v", err)
	}
	if _, err := service.Deactivate(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	replayed, err = service.CommitSend(ctx, prepared.ID, "send-1", true)
	if err != nil || replayed.MessageID != result.MessageID {
		t.Fatalf("inactive replay: result=%#v err=%v", replayed, err)
	}

	stateDir := filepath.Join(paths.Accounts, second.ID)
	if _, err := service.RemoveAccount(ctx, second.ID, true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := filepath.Glob(filepath.Join(stateDir, "*")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Account(second.ID); appErrorCode(err) != api.CodeNotFound {
		t.Fatalf("removed account lookup: %v", err)
	}
}

func TestUncertainSendIsPersistedAndNeverRetried(t *testing.T) {
	ctx := context.Background()
	paths := testPaths(t)
	var sends atomic.Int32
	factory := func(runtime driver.AccountRuntime) (driver.Driver, error) {
		return &uncertainDriver{Driver: fake.New(driver.PlatformWeChat), sends: &sends}, nil
	}
	service, err := New(paths, map[driver.Platform]driver.Factory{driver.PlatformWeChat: factory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	item, err := service.AddAccount("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Activate(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	prepared, err := service.PrepareSend(ctx, item.ID, driver.SendRequest{ConversationID: "conv-file-transfer", Text: "maybe"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.CommitSend(ctx, prepared.ID, "uncertain-1", true)
	if appErrorCode(err) != api.CodeSendUncertain || !result.Uncertain {
		t.Fatalf("first uncertain commit: result=%#v err=%v", result, err)
	}
	result, err = service.CommitSend(ctx, prepared.ID, "uncertain-1", true)
	if appErrorCode(err) != api.CodeSendUncertain || !result.Uncertain {
		t.Fatalf("replayed uncertain commit: result=%#v err=%v", result, err)
	}
	if got := sends.Load(); got != 1 {
		t.Fatalf("driver Send called %d times, want 1", got)
	}
}

func TestAccountDeletionRetriesAfterRestart(t *testing.T) {
	ctx := context.Background()
	paths := testPaths(t)
	var purgeCalls atomic.Int32
	factory := func(driver.AccountRuntime) (driver.Driver, error) {
		return &flakyPurgeDriver{Driver: fake.New(driver.PlatformWeChat), calls: &purgeCalls}, nil
	}
	factories := map[driver.Platform]driver.Factory{driver.PlatformWeChat: factory}

	current, err := New(paths, factories)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if current != nil {
			_ = current.Close(context.Background())
		}
	})
	item, err := current.AddAccount("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(paths.Runtime, "accounts", item.ID)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := current.RemoveAccount(ctx, item.ID, true, true); appErrorCode(err) != api.CodeConflict {
		t.Fatalf("first removal error = %v, want retryable conflict", err)
	} else {
		var appErr *api.AppError
		if !errors.As(err, &appErr) || appErr.Details["deleting"] != true || appErr.Details["retryable"] != true {
			t.Fatalf("first removal did not expose retry details: %#v", appErr)
		}
	}
	listed := current.Accounts()
	if len(listed) != 1 || !listed[0].Deleting || listed[0].LastError == "" {
		t.Fatalf("failed deletion was not retained in the account list: %#v", listed)
	}
	if _, err := current.Account(item.ID); appErrorCode(err) != api.CodeConflict {
		t.Fatalf("deleting account lookup error = %v, want conflict", err)
	}
	if _, err := current.Activate(ctx, item.ID); appErrorCode(err) != api.CodeConflict {
		t.Fatalf("deleting account activation error = %v, want conflict", err)
	}

	if err := current.Close(ctx); err != nil {
		t.Fatal(err)
	}
	current = nil
	reopened, err := New(paths, factories)
	if err != nil {
		t.Fatal(err)
	}
	current = reopened
	listed = reopened.Accounts()
	if len(listed) != 1 || !listed[0].Deleting {
		t.Fatalf("deletion marker did not survive service restart: %#v", listed)
	}
	if _, err := reopened.RemoveAccount(ctx, item.ID, true, true); err != nil {
		t.Fatal(err)
	}
	if got := purgeCalls.Load(); got != 2 {
		t.Fatalf("purge called %d times, want 2", got)
	}
	if len(reopened.Accounts()) != 0 {
		t.Fatalf("account remains after successful retry: %#v", reopened.Accounts())
	}
	if _, err := reopened.Account(item.ID); appErrorCode(err) != api.CodeNotFound {
		t.Fatalf("removed account lookup error = %v, want not found", err)
	}
	for _, path := range []string{filepath.Join(paths.Accounts, item.ID), runtimeDir} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deleted account path %s still exists: %v", path, err)
		}
	}
}

func TestAccountDeletionRequiresBothGuards(t *testing.T) {
	service, err := New(testPaths(t), fakeFactories())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	item, err := service.AddAccount("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RemoveAccount(context.Background(), item.ID, true, false); appErrorCode(err) != api.CodeConfirmationRequired {
		t.Fatalf("removal without confirmation error = %v", err)
	}
	if _, err := service.RemoveAccount(context.Background(), item.ID, false, true); appErrorCode(err) != api.CodeInvalidArgument {
		t.Fatalf("removal without purge error = %v", err)
	}
	if _, err := service.RemoveAccount(context.Background(), item.Alias, true, true); appErrorCode(err) != api.CodeInvalidArgument {
		t.Fatalf("removal by alias error = %v, want invalid argument", err)
	}
	listed := service.Accounts()
	if len(listed) != 1 || listed[0].Deleting {
		t.Fatalf("rejected removal changed account state: %#v", listed)
	}
}

func TestBeginAuthRejectsUnsafeLANAddress(t *testing.T) {
	service, err := New(testPaths(t), fakeFactories())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	item, err := service.AddAccount("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.BeginAuth(context.Background(), item.ID, true, "0.0.0.0"); appErrorCode(err) != api.CodeInvalidArgument {
		t.Fatalf("unsafe LAN address error = %v, want INVALID_ARGUMENT", err)
	}
	unchanged, err := service.Account(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Active {
		t.Fatal("invalid LAN request activated the account before validation")
	}
}

func TestListMessagesRejectsLatestWithNonzeroCursor(t *testing.T) {
	control, err := New(testPaths(t), fakeFactories())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close(context.Background()) })

	_, err = control.ListMessages(context.Background(), "missing-account", driver.MessageQuery{
		AfterSequence: 42,
		Latest:        true,
	})
	if appErrorCode(err) != api.CodeInvalidArgument {
		t.Fatalf("latest read with cursor error = %v, want INVALID_ARGUMENT", err)
	}
}

func TestAccountDeletionBlocksIndexReadersAndDoesNotRecreateState(t *testing.T) {
	ctx := context.Background()
	paths := testPaths(t)
	purgeEntered := make(chan struct{})
	purgeRelease := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(purgeRelease) }) })
	factory := func(driver.AccountRuntime) (driver.Driver, error) {
		return &blockingPurgeDriver{
			Driver:  fake.New(driver.PlatformWeChat),
			entered: purgeEntered,
			release: purgeRelease,
		}, nil
	}
	control, err := New(paths, map[driver.Platform]driver.Factory{driver.PlatformWeChat: factory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close(context.Background()) })
	item, err := control.AddAccount("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.ListMessages(ctx, item.ID, driver.MessageQuery{Limit: 1}); err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(paths.Runtime, "accounts", item.ID)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	removeDone := make(chan error, 1)
	go func() {
		_, err := control.RemoveAccount(ctx, item.ID, true, true)
		removeDone <- err
	}()
	select {
	case <-purgeEntered:
	case <-time.After(time.Second):
		t.Fatal("account purge did not start")
	}

	readStarted := make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		close(readStarted)
		_, err := control.SearchMessages(ctx, item.ID, "fixture", 1)
		readDone <- err
	}()
	<-readStarted
	select {
	case err := <-readDone:
		t.Fatalf("index read bypassed deletion lease: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(purgeRelease) })
	if err := <-removeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-readDone; appErrorCode(err) != api.CodeNotFound {
		t.Fatalf("post-deletion read error = %v, want not found", err)
	}
	for _, path := range []string{filepath.Join(paths.Accounts, item.ID), runtimeDir} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("concurrent read recreated deleted path %s: %v", path, err)
		}
	}
}

func TestPrepareStagesImmutableAttachmentsAndCleansThem(t *testing.T) {
	ctx := context.Background()
	paths := testPaths(t)
	sentContents := make(chan []byte, 1)
	factory := func(driver.AccountRuntime) (driver.Driver, error) {
		return &attachmentCaptureDriver{Driver: fake.New(driver.PlatformWeChat), sentContents: sentContents}, nil
	}
	control, err := New(paths, map[driver.Platform]driver.Factory{driver.PlatformWeChat: factory})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = control.Close(context.Background())
		}
	})
	item, err := control.AddAccount("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.Activate(ctx, item.ID); err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(t.TempDir(), "original.txt")
	original := []byte("immutable attachment contents")
	if err := os.WriteFile(source, original, 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := control.PrepareSend(ctx, item.ID, driver.SendRequest{
		ConversationID: "conv-file-transfer",
		Attachments: []driver.Attachment{{
			Kind: "spoofed", Name: "spoofed.txt", Size: 1, MediaType: "application/x-spoofed", LocalPath: source,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := sha256.Sum256(original)
	if len(prepared.Attachments) != 1 || prepared.Attachments[0].Name != "original.txt" ||
		prepared.Attachments[0].Size != int64(len(original)) || prepared.Attachments[0].SHA256 != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("attachment preview trusts caller metadata or has wrong digest: %#v", prepared.Attachments)
	}
	encoded, err := json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), source) || strings.Contains(string(encoded), ".send-transactions") {
		t.Fatalf("prepared preview leaked a local staging path: %s", encoded)
	}

	control.transactionsMu.Lock()
	stageDir := control.transactions[prepared.ID].StageDir
	stagedPath := control.transactions[prepared.ID].Attachments[0].LocalPath
	control.transactionsMu.Unlock()
	if info, err := os.Stat(stageDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("staging directory permissions: info=%v err=%v", info, err)
	}
	if info, err := os.Stat(stagedPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("staged attachment permissions: info=%v err=%v", info, err)
	}
	if err := os.WriteFile(source, []byte("changed after preview"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := control.CommitSend(ctx, prepared.ID, "immutable-attachment", true)
	if err != nil || !result.Verified {
		t.Fatalf("commit immutable attachment: result=%#v err=%v", result, err)
	}
	if got := <-sentContents; string(got) != string(original) {
		t.Fatalf("driver received mutable source contents %q, want %q", got, original)
	}
	if _, err := os.Stat(stageDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal send staging still exists: %v", err)
	}

	symlink := filepath.Join(t.TempDir(), "attachment-link")
	if err := os.Symlink(source, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := control.PrepareSend(ctx, item.ID, driver.SendRequest{
		ConversationID: "conv-file-transfer", Attachments: []driver.Attachment{{LocalPath: symlink}},
	}); appErrorCode(err) != api.CodeInvalidArgument {
		t.Fatalf("symlink attachment was not rejected: %v", err)
	}

	expiring, err := control.PrepareSend(ctx, item.ID, driver.SendRequest{
		ConversationID: "conv-file-transfer", Attachments: []driver.Attachment{{LocalPath: source}},
	})
	if err != nil {
		t.Fatal(err)
	}
	control.transactionsMu.Lock()
	expiringTransaction := control.transactions[expiring.ID]
	expiringStageDir := expiringTransaction.StageDir
	expiringTransaction.Preview.ExpiresAt = time.Now().UTC().Add(-time.Second)
	control.transactions[expiring.ID] = expiringTransaction
	control.pruneTransactionsLocked()
	control.transactionsMu.Unlock()
	if _, err := os.Stat(expiringStageDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired transaction staging still exists: %v", err)
	}

	shutdown, err := control.PrepareSend(ctx, item.ID, driver.SendRequest{
		ConversationID: "conv-file-transfer", Attachments: []driver.Attachment{{LocalPath: source}},
	})
	if err != nil {
		t.Fatal(err)
	}
	control.transactionsMu.Lock()
	shutdownStageDir := control.transactions[shutdown.ID].StageDir
	control.transactionsMu.Unlock()
	if err := control.Close(ctx); err != nil {
		t.Fatal(err)
	}
	closed = true
	if _, err := os.Stat(shutdownStageDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shutdown transaction staging still exists: %v", err)
	}
}

func TestDurableReservationPreventsRetryAfterRestart(t *testing.T) {
	ctx := context.Background()
	paths := testPaths(t)
	var sends atomic.Int32
	factory := func(driver.AccountRuntime) (driver.Driver, error) {
		return &countingDriver{Driver: fake.New(driver.PlatformWeChat), sends: &sends}, nil
	}
	first, err := New(paths, map[driver.Platform]driver.Factory{driver.PlatformWeChat: factory})
	if err != nil {
		t.Fatal(err)
	}
	item, err := first.AddAccount("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Activate(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	prepared, err := first.PrepareSend(ctx, item.ID, driver.SendRequest{ConversationID: "conv-file-transfer", Text: "once"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := first.runtimes.OpenIndex(item)
	if err != nil {
		t.Fatal(err)
	}
	reservation := driver.SendResult{Uncertain: true, Detail: "simulated crash reservation"}
	if err := store.ReserveSend(ctx, "crash-key", prepared.ID, prepared.RequestHash, reservation); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}

	second, err := New(paths, map[driver.Platform]driver.Factory{driver.PlatformWeChat: factory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	result, err := second.CommitSend(ctx, prepared.ID, "crash-key", true)
	if appErrorCode(err) != api.CodeSendUncertain || !result.Uncertain {
		t.Fatalf("reserved retry: result=%#v err=%v", result, err)
	}
	if got := sends.Load(); got != 0 {
		t.Fatalf("driver Send called %d times after durable crash reservation", got)
	}
	if _, err := second.CommitSend(ctx, prepared.ID, "another-key", true); appErrorCode(err) != api.CodeConflict {
		t.Fatalf("reserved transaction accepted a different key: %v", err)
	}
}

func TestCommitKeepsStagingOwnedPastPreviewExpiry(t *testing.T) {
	ctx := context.Background()
	paths := testPaths(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	factory := func(driver.AccountRuntime) (driver.Driver, error) {
		return &blockingAttachmentDriver{Driver: fake.New(driver.PlatformWeChat), entered: entered, release: release}, nil
	}
	control, err := New(paths, map[driver.Platform]driver.Factory{driver.PlatformWeChat: factory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close(context.Background()) })
	item, err := control.AddAccount("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.Activate(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "attachment.txt")
	if err := os.WriteFile(source, []byte("owned while committing"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := control.PrepareSend(ctx, item.ID, driver.SendRequest{
		ConversationID: "conv-file-transfer", Attachments: []driver.Attachment{{LocalPath: source}},
	})
	if err != nil {
		t.Fatal(err)
	}
	control.transactionsMu.Lock()
	stageDir := control.transactions[prepared.ID].StageDir
	control.transactionsMu.Unlock()

	type commitOutcome struct {
		result driver.SendResult
		err    error
	}
	committed := make(chan commitOutcome, 1)
	go func() {
		result, err := control.CommitSend(ctx, prepared.ID, "expiry-boundary", true)
		committed <- commitOutcome{result: result, err: err}
	}()
	<-entered
	control.transactionsMu.Lock()
	transaction := control.transactions[prepared.ID]
	transaction.Preview.ExpiresAt = time.Now().UTC().Add(-time.Second)
	control.transactions[prepared.ID] = transaction
	control.pruneTransactionsLocked()
	control.transactionsMu.Unlock()
	if _, err := os.Stat(stageDir); err != nil {
		t.Fatalf("reaper removed staging from an accepted commit: %v", err)
	}
	close(release)
	outcome := <-committed
	if outcome.err != nil || !outcome.result.Verified {
		t.Fatalf("commit crossing expiry: result=%#v err=%v", outcome.result, outcome.err)
	}
	if _, err := os.Stat(stageDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal commit staging still exists: %v", err)
	}
}

func TestDefinitePreActionFailureDeletesReservation(t *testing.T) {
	ctx := context.Background()
	paths := testPaths(t)
	var sends atomic.Int32
	factory := func(driver.AccountRuntime) (driver.Driver, error) {
		return &failBeforeActionDriver{Driver: fake.New(driver.PlatformWeChat), sends: &sends}, nil
	}
	control, err := New(paths, map[driver.Platform]driver.Factory{driver.PlatformWeChat: factory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close(context.Background()) })
	item, err := control.AddAccount("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.Activate(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	prepared, err := control.PrepareSend(ctx, item.ID, driver.SendRequest{ConversationID: "conv-file-transfer", Text: "retry-safe"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.CommitSend(ctx, prepared.ID, "retry-safe-key", true); appErrorCode(err) != api.CodePartialFailure {
		t.Fatalf("first pre-action failure: %v", err)
	}
	result, err := control.CommitSend(ctx, prepared.ID, "retry-safe-key", true)
	if err != nil || !result.Verified {
		t.Fatalf("safe retry: result=%#v err=%v", result, err)
	}
	if got := sends.Load(); got != 2 {
		t.Fatalf("driver Send called %d times, want one failed pre-action attempt and one success", got)
	}
}

func TestAuthRequiredPreActionSendIsClassified(t *testing.T) {
	ctx := context.Background()
	paths := testPaths(t)
	factory := func(driver.AccountRuntime) (driver.Driver, error) {
		return &authRequiredSendDriver{Driver: fake.New(driver.PlatformWeChat)}, nil
	}
	control, err := New(paths, map[driver.Platform]driver.Factory{driver.PlatformWeChat: factory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close(context.Background()) })
	item, err := control.AddAccount("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.Activate(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	prepared, err := control.PrepareSend(ctx, item.ID, driver.SendRequest{ConversationID: "conv-file-transfer", Text: "login first"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.CommitSend(ctx, prepared.ID, "auth-required", true); appErrorCode(err) != api.CodeAuthRequired {
		t.Fatalf("pre-action authentication failure = %v, want auth required", err)
	}
	store, err := control.runtimes.OpenIndex(item)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if result, err := store.GetSendResult(ctx, "auth-required", prepared.RequestHash); err != nil {
		t.Fatal(err)
	} else if result != nil {
		t.Fatalf("definite pre-action failure retained a send reservation: %#v", result)
	}
}

func TestHighRiskSurfaceDriverErrorIsClassified(t *testing.T) {
	ctx := context.Background()
	factory := func(driver.AccountRuntime) (driver.Driver, error) {
		return &highRiskSurfaceDriver{Driver: fake.New(driver.PlatformWeChat)}, nil
	}
	control, err := New(testPaths(t), map[driver.Platform]driver.Factory{driver.PlatformWeChat: factory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close(context.Background()) })
	item, err := control.AddAccount("personal", driver.PlatformWeChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.Activate(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	_, err = control.ActSurface(ctx, item.ID, "surface-risk", driver.SurfaceAction{ActionID: "continue"})
	if appErrorCode(err) != api.CodeUserActionRequired {
		t.Fatalf("high-risk surface action error = %v, want user action required", err)
	}
}

type uncertainDriver struct {
	*fake.Driver
	sends *atomic.Int32
}

type flakyPurgeDriver struct {
	*fake.Driver
	calls *atomic.Int32
}

type blockingPurgeDriver struct {
	*fake.Driver
	entered chan<- struct{}
	release <-chan struct{}
}

func (d *blockingPurgeDriver) Purge(ctx context.Context, runtime driver.AccountRuntime) error {
	close(d.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.release:
		return d.Driver.Purge(ctx, runtime)
	}
}

func (d *flakyPurgeDriver) Purge(context.Context, driver.AccountRuntime) error {
	if d.calls.Add(1) == 1 {
		return errors.New("fixture purge failed")
	}
	return nil
}

type attachmentCaptureDriver struct {
	*fake.Driver
	sentContents chan<- []byte
}

func (d *attachmentCaptureDriver) Send(ctx context.Context, request driver.SendRequest) (driver.SendResult, error) {
	if len(request.Attachments) != 1 {
		return driver.SendResult{}, errors.New("fixture expected one attachment")
	}
	contents, err := os.ReadFile(request.Attachments[0].LocalPath)
	if err != nil {
		return driver.SendResult{}, err
	}
	d.sentContents <- contents
	return d.Driver.Send(ctx, request)
}

type countingDriver struct {
	*fake.Driver
	sends *atomic.Int32
}

type blockingAttachmentDriver struct {
	*fake.Driver
	entered chan<- struct{}
	release <-chan struct{}
}

func (d *blockingAttachmentDriver) Send(ctx context.Context, request driver.SendRequest) (driver.SendResult, error) {
	close(d.entered)
	select {
	case <-ctx.Done():
		return driver.SendResult{}, ctx.Err()
	case <-d.release:
	}
	if len(request.Attachments) != 1 {
		return driver.SendResult{}, errors.New("fixture expected one attachment")
	}
	if _, err := os.ReadFile(request.Attachments[0].LocalPath); err != nil {
		return driver.SendResult{}, err
	}
	return d.Driver.Send(ctx, request)
}

func (d *countingDriver) Send(ctx context.Context, request driver.SendRequest) (driver.SendResult, error) {
	d.sends.Add(1)
	return d.Driver.Send(ctx, request)
}

type failBeforeActionDriver struct {
	*fake.Driver
	sends *atomic.Int32
}

type authRequiredSendDriver struct {
	*fake.Driver
}

func (d *authRequiredSendDriver) Send(context.Context, driver.SendRequest) (driver.SendResult, error) {
	return driver.SendResult{}, driver.NewFailure(driver.FailureAuthRequired, "fixture requires authentication")
}

type highRiskSurfaceDriver struct {
	*fake.Driver
}

func (d *highRiskSurfaceDriver) ActSurface(context.Context, string, driver.SurfaceAction) (driver.Surface, error) {
	return driver.Surface{}, driver.NewFailure(driver.FailureUserActionRequired, "fixture requires direct user interaction")
}

func (d *failBeforeActionDriver) Send(ctx context.Context, request driver.SendRequest) (driver.SendResult, error) {
	if d.sends.Add(1) == 1 {
		return driver.SendResult{}, errors.New("fixture failed before touching the UI")
	}
	return d.Driver.Send(ctx, request)
}

func (d *uncertainDriver) Send(context.Context, driver.SendRequest) (driver.SendResult, error) {
	d.sends.Add(1)
	return driver.SendResult{Uncertain: true, Detail: "fixture"}, errors.New("fixture could not verify")
}

func testPaths(t *testing.T) config.Paths {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "state")
	runtime := filepath.Join(root, "runtime")
	return config.Paths{
		Home: home, Accounts: filepath.Join(home, "accounts"), Downloads: filepath.Join(home, "downloads"),
		Runtime: runtime, Socket: filepath.Join(runtime, "daemon.sock"), Registry: filepath.Join(home, "accounts.json"),
	}
}

func fakeFactories() map[driver.Platform]driver.Factory {
	return map[driver.Platform]driver.Factory{
		driver.PlatformWeChat: func(driver.AccountRuntime) (driver.Driver, error) {
			return fake.New(driver.PlatformWeChat), nil
		},
		driver.PlatformWeCom: func(driver.AccountRuntime) (driver.Driver, error) {
			return fake.New(driver.PlatformWeCom), nil
		},
	}
}

func appErrorCode(err error) string {
	var appErr *api.AppError
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	return ""
}
