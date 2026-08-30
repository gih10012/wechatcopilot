package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
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
	phase := 0
	verificationPolls := 0
	actions := make([]CompanionAction, 0, 3)
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
			actions = append(actions, action)
			switch action.Kind {
			case ActionOpenNotification:
				phase = 1
			case ActionSetText:
				phase = 2
			case ActionClick:
				phase = 3
			}
			sequence := int64(10 + phase)
			mu.Unlock()
			_ = json.NewEncoder(writer).Encode(ActionResult{Accepted: true, Sequence: sequence})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/snapshot":
			mu.Lock()
			currentPhase := phase
			if currentPhase >= 3 {
				verificationPolls++
			}
			poll := verificationPolls
			mu.Unlock()
			composerText := ""
			withSend := false
			withNewBubble := false
			if currentPhase == 2 {
				composerText = message
				withSend = true
			}
			if currentPhase >= 3 && poll >= 2 {
				withNewBubble = true
			}
			_ = json.NewEncoder(writer).Encode(testConversationFrame(
				int64(10+currentPhase), title, message, composerText, withSend, withNewBubble,
			))
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
		android: AndroidContainer{
			DockerBinary: "docker", Container: "synthetic",
			Executor: functionExecutor(func(context.Context, string, ...string) ([]byte, error) {
				return resumedActivityOutput(weComConversationActivity), nil
			}),
			Verify: func(context.Context) error { return nil },
		},
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
	initialActions, polls := len(actions), verificationPolls
	recorded := append([]CompanionAction(nil), actions...)
	mu.Unlock()
	if initialActions != 3 || polls < 3 {
		t.Fatalf("baseline was not respected: actions=%d verification_polls=%d", initialActions, polls)
	}
	if recorded[0].Kind != ActionOpenNotification || recorded[1].Kind != ActionSetText ||
		recorded[1].NodeID != "0/2/0" || recorded[1].Text != message || recorded[1].ExpectedSequence != 11 ||
		recorded[2].Kind != ActionClick || recorded[2].NodeID != "0/2/1" || recorded[2].ExpectedSequence != 12 {
		t.Fatalf("send actions were not bound to the verified frame: %#v", recorded)
	}

	memoized, err := driver.Send(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if memoized != result {
		t.Fatalf("memoized result changed: first=%#v second=%#v", result, memoized)
	}
	mu.Lock()
	actionsAfterMemo := len(actions)
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

func testConversationFrame(
	sequence int64,
	title string,
	oldText string,
	composerText string,
	withSend bool,
	withNewBubble bool,
) UISnapshot {
	nodes := []Node{
		{ID: "0", ClassName: "android.widget.FrameLayout", VisibleToUser: true, Bounds: Bounds{Left: 0, Top: 0, Right: 1080, Bottom: 1920}},
		{ID: "0/0", ParentID: "0", ClassName: "android.widget.FrameLayout", VisibleToUser: true, Bounds: Bounds{Left: 0, Top: 0, Right: 1080, Bottom: 160}},
		{ID: "0/0/0", ParentID: "0/0", ClassName: "android.widget.TextView", Text: title, VisibleToUser: true, Bounds: Bounds{Left: 320, Top: 45, Right: 760, Bottom: 115}},
		{ID: "0/1", ParentID: "0", ClassName: "android.widget.ListView", Scrollable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 0, Top: 160, Right: 1080, Bottom: 1680}},
		{ID: "0/1/0", ParentID: "0/1", ClassName: "android.view.ViewGroup", VisibleToUser: true, Bounds: Bounds{Left: 650, Top: 320, Right: 1040, Bottom: 420}},
		{ID: "0/1/0/0", ParentID: "0/1/0", ClassName: "android.widget.TextView", Text: oldText, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 690, Top: 340, Right: 1010, Bottom: 400}},
		{ID: "0/2", ParentID: "0", ClassName: "android.widget.LinearLayout", VisibleToUser: true, Bounds: Bounds{Left: 0, Top: 1680, Right: 1080, Bottom: 1920}},
		{ID: "0/2/0", ParentID: "0/2", ClassName: "android.widget.EditText", ViewID: DefaultWeComPackage + ":id/composer", Text: composerText, Editable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 80, Top: 1740, Right: 850, Bottom: 1840}},
	}
	if withSend {
		nodes = append(nodes, Node{
			ID: "0/2/1", ParentID: "0/2", ClassName: "android.widget.Button", ViewID: DefaultWeComPackage + ":id/send",
			Text: "发送", Clickable: true, Enabled: true, VisibleToUser: true,
			Bounds: Bounds{Left: 880, Top: 1750, Right: 1040, Bottom: 1830},
		})
	}
	if withNewBubble {
		nodes = append(nodes,
			Node{ID: "0/1/1", ParentID: "0/1", ClassName: "android.view.ViewGroup", VisibleToUser: true, Bounds: Bounds{Left: 650, Top: 500, Right: 1040, Bottom: 600}},
			Node{ID: "0/1/1/0", ParentID: "0/1/1", ClassName: "android.widget.TextView", Text: oldText, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 690, Top: 520, Right: 1010, Bottom: 580}},
		)
	}
	return UISnapshot{
		Sequence: sequence, PackageName: DefaultWeComPackage, WindowID: 7,
		WindowTitle: title, WindowClass: weComConversationActivity,
		CapturedAt: time.Now().UTC(), Nodes: nodes,
	}
}

func TestSendReturnsMemoizedUncertainWhenOnlyAnIncomingBubbleAppears(t *testing.T) {
	const (
		token           = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
		accountID       = "account-1"
		title           = "Project"
		message         = "ambiguous same text"
		conversationKey = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	var mu sync.Mutex
	phase := 0
	actionCount := 0
	event := CompanionEvent{
		Sequence: 9, Kind: "notification", PackageName: DefaultWeComPackage,
		ConversationKey: conversationKey, Conversation: title, Openable: true, PostedAt: time.Now().UTC(),
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
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
			switch action.Kind {
			case ActionOpenNotification:
				phase = 1
			case ActionSetText:
				phase = 2
			case ActionClick:
				phase = 3
			}
			sequence := int64(30 + phase)
			mu.Unlock()
			_ = json.NewEncoder(writer).Encode(ActionResult{Accepted: true, Sequence: sequence})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/snapshot":
			mu.Lock()
			currentPhase := phase
			mu.Unlock()
			frame := testConversationFrame(int64(30+currentPhase), title, message, "", false, currentPhase >= 3)
			if currentPhase == 2 {
				frame = testConversationFrame(32, title, message, message, true, false)
			}
			if currentPhase >= 3 {
				// The only new exact-text bubble is left aligned. It could be a
				// concurrent incoming message and must never verify our send.
				frame.Nodes[len(frame.Nodes)-2].Bounds = Bounds{Left: 40, Top: 500, Right: 430, Bottom: 600}
				frame.Nodes[len(frame.Nodes)-1].Bounds = Bounds{Left: 70, Top: 520, Right: 400, Bottom: 580}
			}
			_ = json.NewEncoder(writer).Encode(frame)
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
		android: AndroidContainer{
			DockerBinary: "docker", Container: "synthetic",
			Executor: functionExecutor(func(context.Context, string, ...string) ([]byte, error) {
				return resumedActivityOutput(weComConversationActivity), nil
			}),
			Verify: func(context.Context) error { return nil },
		},
	}
	driver := &Driver{
		runtime: runtime, account: runtime.account,
		surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo),
	}
	request := core.SendRequest{
		ConversationID: conversationID(accountID, conversationKey), Text: message, IdempotencyKey: "send-uncertain",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	result, err := driver.Send(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Uncertain || result.Verified || result.Detail != ErrSendUncertain.Error() {
		t.Fatalf("directionally unproven result = %#v", result)
	}
	mu.Lock()
	beforeMemo := actionCount
	mu.Unlock()
	memoized, err := driver.Send(context.Background(), request)
	if err != nil || memoized != result {
		t.Fatalf("uncertain result was not memoized: result=%#v err=%v", memoized, err)
	}
	mu.Lock()
	afterMemo := actionCount
	mu.Unlock()
	if afterMemo != beforeMemo || beforeMemo != 3 {
		t.Fatalf("uncertain send was replayed: before=%d after=%d", beforeMemo, afterMemo)
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

func TestActSurfaceRechecksAuthenticationMarkersBeforeMutation(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	var actionCalls atomic.Int32
	snapshot := englishWeComLoginMethodSnapshot(false)
	snapshot.WindowID = 7
	snapshot.WindowClass = ""
	executor := &recordingExecutor{output: resumedActivityOutput(weComLoginWxAuthActivity)}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/snapshot":
			_ = json.NewEncoder(writer).Encode(snapshot)
		case request.Method == http.MethodPost && request.URL.Path == "/v1/actions":
			actionCalls.Add(1)
			_ = json.NewEncoder(writer).Encode(ActionResult{Accepted: true, Sequence: snapshot.Sequence})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := newCompanionClientForURL(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	driver := &Driver{
		runtime: &Runtime{
			config: DefaultConfig(), companion: client, running: true,
			android: AndroidContainer{
				DockerBinary: "docker", Container: "synthetic", Executor: executor,
				Verify: func(context.Context) error { return nil },
			},
		},
		surfaces: map[string]surfaceState{
			"surface-1": {
				surface: core.Surface{ID: "surface-1"},
				actions: map[string]surfaceActionState{
					"action-1": {
						advertised: core.Action{ID: "action-1", Kind: ActionClick, Risk: "low", Effect: "navigate"},
						companion:  CompanionAction{Kind: ActionClick, NodeID: "0/0"},
					},
				},
				sequence: snapshot.Sequence,
				identity: surfaceIdentity{
					packageName: DefaultWeComPackage, windowID: 7, windowClass: testConversationActivity,
				},
			},
		},
		sendMemos: make(map[string]sendMemo),
	}
	_, err = driver.ActSurface(context.Background(), "surface-1", core.SurfaceAction{ActionID: "action-1"})
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("authentication surface mutation error = %v, want ErrAuthRequired", err)
	}
	if actionCalls.Load() != 0 {
		t.Fatalf("authentication surface mutation reached companion %d times", actionCalls.Load())
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
