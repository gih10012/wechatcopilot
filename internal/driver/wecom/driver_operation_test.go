package wecom

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	core "github.com/gih10012/wechatcopilot/internal/driver"
)

func TestSendUsesBaselineAndMemoizesVerifiedResult(t *testing.T) {
	const (
		token           = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
		accountID       = "account-1"
		title           = "Project"
		message         = "same text as an older bubble"
		conversationKey = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	var mu sync.Mutex
	actionCount := 0
	verificationPolls := 0
	event := CompanionEvent{
		Sequence: 7, Kind: "notification", PackageName: DefaultWeComPackage,
		ConversationKey: conversationKey, Conversation: title, Sender: "Alice", Text: "status", Openable: true, PostedAt: time.Now().UTC(),
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/events":
			after, _ := strconv.ParseInt(request.URL.Query().Get("after"), 10, 64)
			page := EventPage{Complete: after >= event.Sequence, NextCursor: after}
			if after < event.Sequence {
				page.Events = []CompanionEvent{event}
				page.NextCursor = event.Sequence
			}
			_ = json.NewEncoder(writer).Encode(page)
		case request.Method == http.MethodPost && request.URL.Path == "/v1/actions":
			var action CompanionAction
			if err := json.NewDecoder(request.Body).Decode(&action); err != nil {
				http.Error(writer, "bad action", http.StatusBadRequest)
				return
			}
			mu.Lock()
			actionCount++
			sequence := int64(10 + actionCount)
			mu.Unlock()
			_ = json.NewEncoder(writer).Encode(ActionResult{Accepted: true, Sequence: sequence})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/snapshot":
			mu.Lock()
			phase := actionCount
			if phase >= 2 {
				verificationPolls++
			}
			poll := verificationPolls
			mu.Unlock()
			nodes := []Node{
				{ID: "0/old", Text: message, Enabled: true},
				{ID: "0/input", ClassName: "EditText", Editable: true, Enabled: true},
			}
			if phase >= 1 {
				nodes = append(nodes, Node{ID: "0/send", Text: "发送", Clickable: true, Enabled: true})
			}
			if phase >= 2 && poll >= 2 {
				nodes = append(nodes, Node{ID: "0/new", Text: message, Enabled: true})
			}
			_ = json.NewEncoder(writer).Encode(UISnapshot{
				Sequence: int64(10 + phase), PackageName: DefaultWeComPackage,
				WindowTitle: title, CapturedAt: time.Now().UTC(), Nodes: nodes,
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := newCompanionClientForURL(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{
		config: DefaultConfig(), companion: client, running: true,
		account: core.AccountRuntime{AccountID: accountID, Alias: "work"},
	}
	driver := &Driver{
		runtime: runtime, account: runtime.account,
		surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo),
	}
	request := core.SendRequest{
		ConversationID: conversationID(accountID, event.ConversationKey), Text: message, IdempotencyKey: "send-1",
	}
	result, err := driver.Send(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Verified || result.Uncertain {
		t.Fatalf("unexpected send result: %#v", result)
	}
	mu.Lock()
	initialActions, polls := actionCount, verificationPolls
	mu.Unlock()
	if initialActions != 3 || polls < 2 {
		t.Fatalf("baseline was not respected: actions=%d verification_polls=%d", initialActions, polls)
	}

	memoized, err := driver.Send(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if memoized != result {
		t.Fatalf("memoized result changed: first=%#v second=%#v", result, memoized)
	}
	mu.Lock()
	actionsAfterMemo := actionCount
	mu.Unlock()
	if actionsAfterMemo != initialActions {
		t.Fatalf("memoized send repeated UI actions: before=%d after=%d", initialActions, actionsAfterMemo)
	}

	conflict := request
	conflict.Text = "different content"
	if _, err := driver.Send(context.Background(), conflict); err == nil {
		t.Fatal("expected reuse of an idempotency key for different content to fail")
	}
}

func TestOperationMutexSerializesAuthEntries(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	snapshotEntered := make(chan struct{})
	releaseSnapshot := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/snapshot" {
			http.NotFound(writer, request)
			return
		}
		close(snapshotEntered)
		<-releaseSnapshot
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(UISnapshot{
			Sequence: 1, PackageName: DefaultWeComPackage, CapturedAt: time.Now().UTC(),
			Nodes: []Node{{ID: "0/code", Text: "验证码", Editable: true, Enabled: true}},
		})
	}))
	defer server.Close()
	client, err := newCompanionClientForURL(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	png := []byte("\x89PNG\r\n\x1a\nsynthetic")
	runtime := &Runtime{
		config: DefaultConfig(), companion: client, running: true,
		android: AndroidContainer{
			DockerBinary: "docker", Container: "synthetic", Executor: &recordingExecutor{output: png},
			Verify: func(context.Context) error { return nil },
		},
	}
	driver := &Driver{
		runtime: runtime, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo),
	}
	authDone := make(chan error, 1)
	go func() {
		_, err := driver.AuthSnapshot(context.Background())
		authDone <- err
	}()
	select {
	case <-snapshotEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("AuthSnapshot never reached the companion")
	}

	submitDone := make(chan error, 1)
	go func() { submitDone <- driver.SubmitAuthCode(context.Background(), "bad") }()
	select {
	case err := <-submitDone:
		t.Fatalf("SubmitAuthCode interleaved with AuthSnapshot: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseSnapshot)
	if err := <-authDone; err != nil {
		t.Fatal(err)
	}
	if err := <-submitDone; err == nil {
		t.Fatal("expected invalid verification code to be rejected after acquiring the operation lock")
	}
}

func TestStopClearsOperationState(t *testing.T) {
	driver := &Driver{
		runtime:   &Runtime{},
		account:   core.AccountRuntime{AccountID: "account-1"},
		surfaces:  map[string]surfaceState{"surface-1": {}},
		sendMemos: map[string]sendMemo{"send-1": {digest: "digest", result: core.SendResult{Verified: true}}},
	}
	if err := driver.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if driver.account.AccountID != "" || len(driver.surfaces) != 0 || len(driver.sendMemos) != 0 {
		t.Fatalf("operation state survived Stop: account=%q surfaces=%d memos=%d", driver.account.AccountID, len(driver.surfaces), len(driver.sendMemos))
	}
}

func TestAllUIAndLifecycleEntriesShareOperationMutex(t *testing.T) {
	driver := &Driver{
		runtime:   &Runtime{},
		surfaces:  make(map[string]surfaceState),
		sendMemos: make(map[string]sendMemo),
	}
	operations := []struct {
		name string
		call func() error
	}{
		{name: "Start", call: func() error {
			return driver.Start(context.Background(), core.AccountRuntime{AccountID: "../invalid"})
		}},
		{name: "Stop", call: func() error { return driver.Stop(context.Background()) }},
		{name: "Purge", call: func() error {
			return driver.Purge(context.Background(), core.AccountRuntime{AccountID: "../invalid"})
		}},
		{name: "Status", call: func() error {
			_, err := driver.Status(context.Background())
			return err
		}},
		{name: "AuthSnapshot", call: func() error {
			_, err := driver.AuthSnapshot(context.Background())
			return err
		}},
		{name: "SubmitAuthCode", call: func() error {
			return driver.SubmitAuthCode(context.Background(), "bad")
		}},
		{name: "Send", call: func() error {
			_, err := driver.Send(context.Background(), core.SendRequest{})
			return err
		}},
		{name: "OpenSurface", call: func() error {
			_, err := driver.OpenSurface(context.Background(), "")
			return err
		}},
		{name: "SnapshotSurface", call: func() error {
			_, err := driver.SnapshotSurface(context.Background(), "missing")
			return err
		}},
		{name: "ActSurface", call: func() error {
			_, err := driver.ActSurface(context.Background(), "missing", core.SurfaceAction{})
			return err
		}},
		{name: "CloseSurface", call: func() error {
			return driver.CloseSurface(context.Background(), "missing")
		}},
	}

	driver.operationMu.Lock()
	var ready sync.WaitGroup
	ready.Add(len(operations))
	start := make(chan struct{})
	completed := make(chan string, len(operations))
	for _, operation := range operations {
		operation := operation
		go func() {
			ready.Done()
			<-start
			_ = operation.call()
			completed <- operation.name
		}()
	}
	ready.Wait()
	close(start)
	select {
	case name := <-completed:
		driver.operationMu.Unlock()
		t.Fatalf("%s did not acquire the shared operation mutex", name)
	case <-time.After(100 * time.Millisecond):
	}
	driver.operationMu.Unlock()

	deadline := time.After(2 * time.Second)
	for range operations {
		select {
		case <-completed:
		case <-deadline:
			t.Fatal("serialized operation did not complete after releasing the mutex")
		}
	}
}
