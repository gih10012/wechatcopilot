package wecom

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/gih10012/wechatcopilot/internal/driver"
)

func TestNotificationRecordsSatisfySharedIndexContract(t *testing.T) {
	token := "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	event := CompanionEvent{
		Sequence: 7, Kind: "notification", PackageName: DefaultWeComPackage,
		ConversationKey: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Conversation:    "Project", Sender: "Alice", Text: "status update", Openable: true, PostedAt: time.Now().UTC(),
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		after, _ := strconv.ParseInt(request.URL.Query().Get("after"), 10, 64)
		page := EventPage{Complete: false}
		if after < event.Sequence {
			page.Events = []CompanionEvent{event}
			page.NextCursor = event.Sequence
		} else {
			page.Complete = true
		}
		_ = json.NewEncoder(writer).Encode(page)
	}))
	defer server.Close()
	client, err := newCompanionClientForURL(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	runtime := &Runtime{
		config: config, companion: client, running: true,
		account: core.AccountRuntime{AccountID: "account-1", Alias: "work"},
	}
	driver := &Driver{
		runtime: runtime, account: runtime.account,
		surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo),
	}
	conversations, err := driver.ListConversations(context.Background(), core.ConversationQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 1 || conversations[0].ID == "" || conversations[0].ExternalID == "" {
		t.Fatalf("conversation violates core index contract: %#v", conversations)
	}
	messages, err := driver.ReadMessages(context.Background(), core.MessageQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID == "" || messages[0].ExternalID == "" || messages[0].ConversationID != conversations[0].ID {
		t.Fatalf("message violates core index contract: %#v", messages)
	}
}

func TestCapabilitiesUseSharedCompleteContract(t *testing.T) {
	values := capabilities()
	if err := core.ValidateCapabilities(values); err != nil {
		t.Fatal(err)
	}
	if values[core.CapabilityMessagesSend] != core.SupportExperimental {
		t.Fatalf("text-send capability = %q", values[core.CapabilityMessagesSend])
	}
	if values[core.CapabilityMessagesHistory] != core.SupportUnsupported {
		t.Fatalf("history capability = %q", values[core.CapabilityMessagesHistory])
	}
}

func TestClassifyLoginUsesObservedWeComWindowClass(t *testing.T) {
	tests := []struct {
		name        string
		windowClass string
		want        core.RuntimeState
	}{
		{name: "login", windowClass: weComLoginWxAuthActivity, want: core.StateAuthRequired},
		{name: "sms verification", windowClass: weComSMSVerifyActivity, want: core.StateAuthRequired},
		{name: "splash", windowClass: weComLaunchActivity, want: core.StateStarting},
		{name: "post-login scanner", windowClass: "com.tencent.wework.login.controller.LoginScannerActivity", want: core.StateDegraded},
		{name: "gesture settings", windowClass: "com.tencent.wework.login.controller.SettingGestureActivity", want: core.StateDegraded},
		{name: "unknown", windowClass: "com.tencent.wework.unknown.CustomActivity", want: core.StateDegraded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, _ := classifyLogin(UISnapshot{PackageName: DefaultWeComPackage, WindowClass: test.windowClass})
			if state != test.want {
				t.Fatalf("classifyLogin(%q) = %s, want %s", test.windowClass, state, test.want)
			}
		})
	}

	chat := UISnapshot{
		Sequence:    32,
		PackageName: DefaultWeComPackage,
		WindowClass: "com.tencent.wework.msg.controller.ConversationActivity",
		Nodes: []Node{
			{ID: "0/0", Text: "你的短信验证码是 123456", VisibleToUser: true},
			{ID: "0/1", Editable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 20, Top: 100, Right: 300, Bottom: 160}},
		},
	}
	if validSMSAuthSnapshot(chat) {
		t.Fatal("post-login chat text was accepted as an SMS authentication screen")
	}
	state, _ := classifyLogin(chat)
	if state == core.StateAuthRequired {
		t.Fatal("post-login chat text was classified as authentication required")
	}
}

func TestPrivacyConsentActionRequiresExactOfficialScreenAndUniqueTarget(t *testing.T) {
	base := UISnapshot{
		Sequence: 17, PackageName: DefaultWeComPackage, WindowClass: weComLoginWxAuthActivity,
		Nodes: []Node{
			{ID: "0/0", Text: "Privacy Policy", Enabled: true, VisibleToUser: true},
			{ID: "0/1", Text: "Welcome to WeCom!", Enabled: true, VisibleToUser: true},
			{ID: "0/2", Text: "Agree", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 10, Top: 10, Right: 100, Bottom: 50}},
			{ID: "0/3", Text: "Disagree", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 10, Top: 60, Right: 100, Bottom: 100}},
		},
	}
	actions := privacyConsentActions(base)
	if len(actions) != 1 || actions[0].ID != acceptPrivacyPolicyAction || !actions[0].RequiresConfirmation || actions[0].Risk != "high" {
		t.Fatalf("privacy actions = %#v", actions)
	}

	chinese := base
	chinese.WindowClass = weComLaunchActivity
	chinese.Nodes = []Node{
		{ID: "0/0", Text: "隐私政策", Enabled: true, VisibleToUser: true},
		{ID: "0/1", Text: "欢迎使用企业微信", Enabled: true, VisibleToUser: true},
		{ID: "0/2", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 10, Top: 10, Right: 100, Bottom: 50}},
		{ID: "0/2/0", ParentID: "0/2", Text: "同意", Enabled: true, VisibleToUser: true},
		{ID: "0/3", Text: "不同意", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 10, Top: 60, Right: 100, Bottom: 100}},
	}
	target, err := uniquePrivacyConsentTarget(chinese)
	if err != nil || target.ID != "0/2" {
		t.Fatalf("nested Chinese consent target = %#v err=%v", target, err)
	}

	tests := []struct {
		name   string
		mutate func(*UISnapshot)
	}{
		{name: "wrong package", mutate: func(value *UISnapshot) { value.PackageName = "example.invalid" }},
		{name: "unapproved activity", mutate: func(value *UISnapshot) { value.WindowClass = "com.tencent.wework.settings.SettingsActivity" }},
		{name: "missing welcome marker", mutate: func(value *UISnapshot) { value.Nodes[1].Text = "" }},
		{name: "hidden privacy marker", mutate: func(value *UISnapshot) { value.Nodes[0].VisibleToUser = false }},
		{name: "missing disagree control", mutate: func(value *UISnapshot) { value.Nodes[3].Text = "" }},
		{name: "disabled disagree control", mutate: func(value *UISnapshot) { value.Nodes[3].Enabled = false }},
		{name: "hidden disagree control", mutate: func(value *UISnapshot) { value.Nodes[3].VisibleToUser = false }},
		{name: "offscreen disagree control", mutate: func(value *UISnapshot) { value.Nodes[3].Bounds = Bounds{} }},
		{name: "zero sequence", mutate: func(value *UISnapshot) { value.Sequence = 0 }},
		{name: "disabled button", mutate: func(value *UISnapshot) { value.Nodes[2].Enabled = false }},
		{name: "hidden button", mutate: func(value *UISnapshot) { value.Nodes[2].VisibleToUser = false }},
		{name: "negative offscreen button", mutate: func(value *UISnapshot) {
			value.Nodes[2].Bounds = Bounds{Left: -100, Top: -50, Right: -10, Bottom: -5}
		}},
		{name: "offscreen button", mutate: func(value *UISnapshot) { value.Nodes[2].Bounds = Bounds{} }},
		{name: "device verification overlay", mutate: func(value *UISnapshot) {
			value.Nodes = append(value.Nodes, Node{ID: "0/4", Text: "Device verification", Enabled: true})
		}},
		{name: "ambiguous buttons", mutate: func(value *UISnapshot) {
			value.Nodes = append(value.Nodes, Node{ID: "0/4", Text: "Agree", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 110, Top: 10, Right: 200, Bottom: 50}})
		}},
		{name: "ambiguous disagree controls", mutate: func(value *UISnapshot) {
			value.Nodes = append(value.Nodes, Node{ID: "0/4", Text: "Disagree", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 110, Top: 60, Right: 200, Bottom: 100}})
		}},
		{name: "shared clickable branch", mutate: func(value *UISnapshot) {
			value.Nodes[2] = Node{ID: "0/4/0", ParentID: "0/4", Text: "Agree", Enabled: true, VisibleToUser: true}
			value.Nodes[3] = Node{ID: "0/4/1", ParentID: "0/4", Text: "Disagree", Enabled: true, VisibleToUser: true}
			value.Nodes = append(value.Nodes, Node{ID: "0/4", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 10, Top: 10, Right: 200, Bottom: 100}})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base
			value.Nodes = append([]Node(nil), base.Nodes...)
			test.mutate(&value)
			if actions := privacyConsentActions(value); len(actions) != 0 {
				t.Fatalf("unsafe screen advertised actions: %#v", actions)
			}
		})
	}
}

func TestSMSAuthInputRequiresFreshVisibleOfficialLoginScreen(t *testing.T) {
	base := UISnapshot{
		Sequence:    31,
		PackageName: DefaultWeComPackage,
		WindowClass: "com.tencent.wework.login.controller.LoginVeryfyStep2Activity",
		Nodes: []Node{
			{ID: "0/0", Text: "Verification code", Enabled: true, VisibleToUser: true},
			{ID: "0/1", Editable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 20, Top: 100, Right: 300, Bottom: 160}},
		},
	}
	if !validSMSAuthSnapshot(base) {
		t.Fatal("valid official SMS screen was rejected")
	}
	tests := []struct {
		name   string
		mutate func(*UISnapshot)
	}{
		{name: "wrong package", mutate: func(value *UISnapshot) { value.PackageName = "example.invalid" }},
		{name: "missing authoritative activity", mutate: func(value *UISnapshot) { value.WindowClass = "" }},
		{name: "wrong activity package", mutate: func(value *UISnapshot) { value.WindowClass = "com.android.settings.Settings" }},
		{name: "missing code marker", mutate: func(value *UISnapshot) { value.Nodes[0].Text = "Enter value" }},
		{name: "hidden code marker", mutate: func(value *UISnapshot) { value.Nodes[0].VisibleToUser = false }},
		{name: "hidden input", mutate: func(value *UISnapshot) { value.Nodes[1].VisibleToUser = false }},
		{name: "offscreen input", mutate: func(value *UISnapshot) { value.Nodes[1].Bounds = Bounds{} }},
		{name: "multiple inputs", mutate: func(value *UISnapshot) {
			value.Nodes = append(value.Nodes, Node{ID: "0/2", Editable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 20, Top: 180, Right: 300, Bottom: 240}})
		}},
		{name: "risk warning", mutate: func(value *UISnapshot) {
			value.Nodes = append(value.Nodes, Node{ID: "0/2", Text: "Device verification", VisibleToUser: true})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base
			value.Nodes = append([]Node(nil), base.Nodes...)
			test.mutate(&value)
			if validSMSAuthSnapshot(value) {
				t.Fatal("unsafe SMS screen was accepted")
			}
		})
	}
}

func TestPerformPrivacyConsentUsesFreshSemanticTargetAndConfirmation(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	snapshot := UISnapshot{
		Sequence: 23, PackageName: DefaultWeComPackage, WindowClass: weComLoginWxAuthActivity,
		Nodes: []Node{
			{ID: "0/0", Text: "Privacy Policy", Enabled: true, VisibleToUser: true},
			{ID: "0/1", Text: "Welcome to WeCom!", Enabled: true, VisibleToUser: true},
			{ID: "0/2", Text: "Agree", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 10, Top: 10, Right: 100, Bottom: 50}},
			{ID: "0/3", Text: "Disagree", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 10, Top: 60, Right: 100, Bottom: 100}},
		},
	}
	actions := make(chan CompanionAction, 1)
	var sequence atomic.Int64
	var acted atomic.Bool
	var readsAfterAction atomic.Int32
	sequence.Store(snapshot.Sequence)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/snapshot":
			current := snapshot
			current.Sequence = sequence.Load()
			if acted.Load() {
				read := readsAfterAction.Add(1)
				if read >= 2 {
					current.Sequence = snapshot.Sequence + 2
					current.Nodes = []Node{{ID: "0/0", Text: "Log in to WeCom", Enabled: true, VisibleToUser: true}}
				}
			}
			_ = json.NewEncoder(writer).Encode(current)
		case request.Method == http.MethodPost && request.URL.Path == "/v1/actions":
			var action CompanionAction
			if err := json.NewDecoder(request.Body).Decode(&action); err != nil {
				http.Error(writer, "bad action", http.StatusBadRequest)
				return
			}
			actions <- action
			sequence.Store(snapshot.Sequence + 1)
			acted.Store(true)
			_ = json.NewEncoder(writer).Encode(ActionResult{Accepted: true, Sequence: snapshot.Sequence + 1})
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
		android: AndroidContainer{
			DockerBinary: "docker", Container: "synthetic",
			Executor: functionExecutor(func(context.Context, string, ...string) ([]byte, error) {
				return []byte("topResumedActivity=ActivityRecord{x u0 " + DefaultWeComPackage + "/.login.controller.LoginWxAuthActivity}\n"), nil
			}),
			Verify: func(context.Context) error { return nil },
		},
	}
	driverInstance := &Driver{runtime: runtime, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}
	if err := driverInstance.PerformAuthAction(context.Background(), core.AuthActionRequest{ActionID: acceptPrivacyPolicyAction}); !errors.Is(err, ErrUserActionRequired) {
		t.Fatalf("unconfirmed consent error = %v", err)
	}
	select {
	case action := <-actions:
		t.Fatalf("unconfirmed consent reached companion: %#v", action)
	default:
	}
	if err := driverInstance.PerformAuthAction(context.Background(), core.AuthActionRequest{ActionID: acceptPrivacyPolicyAction, Confirmed: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case action := <-actions:
		if action.Kind != ActionClick || action.NodeID != "0/2" || action.ExpectedSequence != snapshot.Sequence || action.Text != "" {
			t.Fatalf("privacy action = %#v", action)
		}
	case <-time.After(time.Second):
		t.Fatal("confirmed consent did not reach companion")
	}
	if readsAfterAction.Load() < 3 {
		t.Fatalf("consent returned before observing stable modal dismissal: reads=%d", readsAfterAction.Load())
	}
}

func TestForegroundActivityOverridesOrClearsCompanionObservation(t *testing.T) {
	snapshot := UISnapshot{
		PackageName: DefaultWeComPackage,
		WindowClass: "com.tencent.wework.login.controller.SettingGestureActivity",
	}
	android := AndroidContainer{
		DockerBinary: "docker", Container: "synthetic",
		Executor: functionExecutor(func(context.Context, string, ...string) ([]byte, error) {
			return []byte("topResumedActivity=ActivityRecord{x u0 " + DefaultWeComPackage + "/.login.controller.LoginWxAuthActivity}\n"), nil
		}),
		Verify: func(context.Context) error { return nil },
	}
	got := withForegroundActivity(context.Background(), android, snapshot)
	if got.WindowClass != weComLoginWxAuthActivity {
		t.Fatalf("foreground activity = %q, want %q", got.WindowClass, weComLoginWxAuthActivity)
	}

	android.Executor = &sequenceExecutor{results: []executorResult{{err: errors.New("probe failed")}}}
	got = withForegroundActivity(context.Background(), android, snapshot)
	if got.WindowClass != "" {
		t.Fatalf("failed authoritative probe retained companion class %q", got.WindowClass)
	}
}

func TestSameTitleNotificationKeysRemainDistinctConversations(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	events := []CompanionEvent{
		{Sequence: 7, Kind: "notification", PackageName: DefaultWeComPackage, ConversationKey: strings.Repeat("a", 64), Conversation: "同名群", Openable: true, PostedAt: time.Now().UTC()},
		{Sequence: 8, Kind: "notification", PackageName: DefaultWeComPackage, ConversationKey: strings.Repeat("b", 64), Conversation: "同名群", Openable: true, PostedAt: time.Now().UTC().Add(time.Second)},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		after, _ := strconv.ParseInt(request.URL.Query().Get("after"), 10, 64)
		page := EventPage{Complete: true, NextCursor: after}
		for _, event := range events {
			if event.Sequence > after {
				page.Events = append(page.Events, event)
				page.NextCursor = event.Sequence
			}
		}
		_ = json.NewEncoder(writer).Encode(page)
	}))
	defer server.Close()
	client, err := newCompanionClientForURL(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{
		config: DefaultConfig(), companion: client, running: true,
		account: core.AccountRuntime{AccountID: "account-1"},
	}
	driver := &Driver{runtime: runtime, account: runtime.account, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}
	conversations, err := driver.ListConversations(context.Background(), core.ConversationQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 2 || conversations[0].ID == conversations[1].ID || conversations[0].Title != conversations[1].Title {
		t.Fatalf("same-title targets were merged or lost provenance: %#v", conversations)
	}
}

func TestConversationKeyMappingMultipleTitlesFailsClosed(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	key := strings.Repeat("c", 64)
	events := []CompanionEvent{
		{Sequence: 7, PackageName: DefaultWeComPackage, ConversationKey: key, Conversation: "Alpha", Openable: true},
		{Sequence: 8, PackageName: DefaultWeComPackage, ConversationKey: key, Conversation: "Beta", Openable: true},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(EventPage{Events: events, NextCursor: 8, Complete: true})
	}))
	defer server.Close()
	client, err := newCompanionClientForURL(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{config: DefaultConfig(), companion: client, running: true, account: core.AccountRuntime{AccountID: "account-1"}}
	driver := &Driver{runtime: runtime, account: runtime.account, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}
	_, err = driver.resolveConversationEvent(context.Background(), conversationID("account-1", key))
	if !errors.Is(err, ErrTargetAmbiguous) {
		t.Fatalf("expected ambiguous stable-key mapping, got %v", err)
	}
}

func TestHighRiskSnapshotDisablesAllMutatingSurfaceActions(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nsynthetic")
	runtime := &Runtime{
		running: true,
		android: AndroidContainer{
			DockerBinary: "docker", Container: "synthetic", Executor: &recordingExecutor{output: png},
			Verify: func(context.Context) error { return nil },
		},
	}
	driver := &Driver{runtime: runtime, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}
	surface, err := driver.updateSurface(context.Background(), "surface-risk", UISnapshot{
		Sequence: 9, PackageName: DefaultWeComPackage, WindowTitle: "订单支付",
		Nodes: []Node{
			{ID: "0/confirm", Text: "确认", Clickable: true, Enabled: true},
			{ID: "0/note", Text: "备注", Editable: true, Enabled: true},
			{ID: "0/list", Text: "详情", Scrollable: true, Enabled: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range surface.Actions {
		if action.Kind == ActionClick || action.Kind == ActionSetText {
			if !action.Disabled || action.Risk != "high" {
				t.Fatalf("high-risk mutating action remained enabled: %#v", action)
			}
		}
	}
	if len(driver.surfaces["surface-risk"].actions) != 2 {
		t.Fatalf("only bounded scroll actions should remain enabled: %#v", driver.surfaces["surface-risk"].actions)
	}
}

func TestOpenSurfaceRejectsGenericUILabelReference(t *testing.T) {
	driver := &Driver{runtime: &Runtime{}, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}
	_, err := driver.OpenSurface(context.Background(), "打开任意当前按钮")
	if !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("expected generic label reference to be rejected, got %v", err)
	}
}
