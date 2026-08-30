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

var authFixturePNG = []byte("\x89PNG\r\n\x1a\nwecom-auth-fixture")

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
	history, err := allEvents(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if history.Complete || len(history.Events) != 1 || history.Events[0].Sequence != event.Sequence {
		t.Fatalf("incomplete event history was discarded or mislabeled: %#v", history)
	}
	if _, err := driver.notificationEvent(context.Background(), client, event.Sequence-1); !errors.Is(err, ErrStale) {
		t.Fatalf("lookup across known journal gap error = %v, want ErrStale", err)
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
	if len(messages) != 1 || messages[0].ID == "" || messages[0].ExternalID == "" ||
		messages[0].ConversationID != conversations[0].ID || !messages[0].GapBefore {
		t.Fatalf("message violates core index contract: %#v", messages)
	}
	encoded, err := json.Marshal(messages[0])
	if err != nil || !strings.Contains(string(encoded), `"gap_before":true`) {
		t.Fatalf("message gap is not exposed compatibly: json=%s err=%v", encoded, err)
	}
}

func TestReadMessagesOmitsGapMarkerForCompleteEventPage(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	event := CompanionEvent{
		Sequence: 9, PackageName: DefaultWeComPackage,
		ConversationKey: strings.Repeat("d", 64), Conversation: "Project", Text: "latest",
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(EventPage{Events: []CompanionEvent{event}, NextCursor: event.Sequence, Complete: true})
	}))
	defer server.Close()
	client, err := newCompanionClientForURL(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{config: DefaultConfig(), companion: client, running: true}
	driver := &Driver{
		runtime: runtime, account: core.AccountRuntime{AccountID: "account-1"},
		surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo),
	}
	messages, err := driver.ReadMessages(context.Background(), core.MessageQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].GapBefore {
		t.Fatalf("complete page gained a false gap marker: %#v", messages)
	}
	encoded, err := json.Marshal(messages[0])
	if err != nil || strings.Contains(string(encoded), "gap_before") {
		t.Fatalf("zero gap marker is not backward-compatible: json=%s err=%v", encoded, err)
	}
}

func TestReadMessagesPreservesConversationScopedGapAfterGlobalFiltering(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	accountID := "account-1"
	keyA := strings.Repeat("a", 64)
	keyB := strings.Repeat("b", 64)
	events := []CompanionEvent{
		{Sequence: 21, PackageName: DefaultWeComPackage, ConversationKey: keyA, Conversation: "A", Text: "a-boundary", GapBefore: true},
		{Sequence: 22, PackageName: DefaultWeComPackage, ConversationKey: keyA, Conversation: "A", Text: "a-next"},
		{Sequence: 23, PackageName: DefaultWeComPackage, ConversationKey: keyB, Conversation: "B", Text: "b-boundary", GapBefore: true},
		{Sequence: 24, PackageName: DefaultWeComPackage, ConversationKey: keyB, Conversation: "B", Text: "b-next"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(EventPage{Events: events, NextCursor: 24, Complete: true})
	}))
	defer server.Close()
	client, err := newCompanionClientForURL(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{config: DefaultConfig(), companion: client, running: true}
	driver := &Driver{
		runtime: runtime, account: core.AccountRuntime{AccountID: accountID},
		surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo),
	}
	messages, err := driver.ReadMessages(context.Background(), core.MessageQuery{
		ConversationID: conversationID(accountID, keyB), Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Text != "b-boundary" || !messages[0].GapBefore || messages[1].GapBefore {
		t.Fatalf("filtered conversation boundary = %#v", messages)
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

	staleBottomNavigation := UISnapshot{
		PackageName: DefaultWeComPackage,
		WindowClass: weComLoginWxAuthActivity,
		Nodes: []Node{
			{ID: "0/agreement", ClassName: androidImageViewClass, ViewID: weComLoginAgreementViewID},
			{ID: "0/messages", Text: "消息", VisibleToUser: true},
			{ID: "0/workbench", Text: "工作台", VisibleToUser: true},
		},
	}
	if state, _ := classifyLogin(staleBottomNavigation); state != core.StateAuthRequired {
		t.Fatalf("login screen with stale bottom navigation = %s, want %s", state, core.StateAuthRequired)
	}
	riskWithStaleBottomNavigation := UISnapshot{
		PackageName: DefaultWeComPackage,
		WindowClass: "com.tencent.wework.login.controller.SecurityVerificationActivity",
		Nodes: []Node{
			{ID: "0/risk", Text: "Device verification", VisibleToUser: true},
			{ID: "0/messages", Text: "消息", VisibleToUser: true},
			{ID: "0/workbench", Text: "工作台", VisibleToUser: true},
		},
	}
	if state, reason := classifyLogin(riskWithStaleBottomNavigation); state != core.StateAuthRequired ||
		!strings.Contains(reason, "direct user action") {
		t.Fatalf("risk Activity with stale bottom navigation = %s/%q, want AUTH_REQUIRED/user action", state, reason)
	}
	ordinaryAgreementMessage := UISnapshot{
		PackageName: DefaultWeComPackage,
		WindowClass: testConversationActivity,
		Nodes: []Node{
			{ID: "0/message", Text: "同意", VisibleToUser: true},
			{ID: "0/messages", Text: "消息", VisibleToUser: true},
			{ID: "0/workbench", Text: "工作台", VisibleToUser: true},
		},
	}
	if state, _ := classifyLogin(ordinaryAgreementMessage); state != core.StateOnline {
		t.Fatalf("ordinary chat agreement text = %s, want %s", state, core.StateOnline)
	}

	evilSuffix := UISnapshot{
		PackageName: DefaultWeComPackage,
		WindowClass: weComLoginWxAuthActivity + "Evil",
	}
	if state, _ := classifyLogin(evilSuffix); state == core.StateAuthRequired {
		t.Fatalf("lookalike login activity was trusted: %s", state)
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
	actions := privacyConsentActions(base, authFixturePNG, "account-1")
	if len(actions) != 1 {
		t.Fatalf("privacy actions = %#v", actions)
	}
	operation, validID := parseAuthActionID(actions[0].ID)
	if !validID || operation != acceptPrivacyPolicyAction || actions[0].ReplayKey != acceptPrivacyPolicyAction || !actions[0].ImageBound ||
		!actions[0].RequiresConfirmation || actions[0].Risk != "high" {
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
			value.Nodes = append(value.Nodes, Node{ID: "0/4", Text: "Device verification", Enabled: true, VisibleToUser: true})
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
			if actions := privacyConsentActions(value, authFixturePNG, "account-1"); len(actions) != 0 {
				t.Fatalf("unsafe screen advertised actions: %#v", actions)
			}
		})
	}
}

func englishWeComLoginMethodSnapshot(selected bool) UISnapshot {
	return UISnapshot{
		Sequence: 41, PackageName: DefaultWeComPackage, WindowClass: weComLoginWxAuthActivity,
		Nodes: []Node{
			{ID: "0/0", Text: "Continue with WeChat", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 20, Top: 300, Right: 340, Bottom: 360}},
			{ID: "0/1", Text: "Continue with Email", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 20, Top: 380, Right: 170, Bottom: 430}},
			{ID: "0/2", Text: "Continue with Phone", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 190, Top: 380, Right: 340, Bottom: 430}},
			{ID: "0/3", ClassName: androidImageViewClass, ViewID: weComLoginAgreementViewID, Clickable: true, Selected: boolPointer(selected), Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 20, Top: 460, Right: 50, Bottom: 490}},
			{ID: "0/4", Text: "Read and Agree Software Licensing and Service Agreements and Privacy Policy", Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 60, Top: 450, Right: 340, Bottom: 510}},
		},
	}
}

func TestWeComLoginMethodActionsRequireExactOfficialScreenAndAgreementState(t *testing.T) {
	unchecked := englishWeComLoginMethodSnapshot(false)
	actions, prompt := authenticationActions(unchecked, authFixturePNG, "account-1")
	if len(actions) != 1 {
		t.Fatalf("unchecked login method actions = %#v prompt=%q", actions, prompt)
	}
	operation, validID := parseAuthActionID(actions[0].ID)
	if !validID || operation != acceptWeComLoginTermsAction || actions[0].ReplayKey != acceptWeComLoginTermsAction || !actions[0].ImageBound ||
		!actions[0].RequiresConfirmation || actions[0].Risk != "high" || prompt == "" {
		t.Fatalf("unchecked login method actions = %#v prompt=%q", actions, prompt)
	}

	checked := englishWeComLoginMethodSnapshot(true)
	actions, prompt = authenticationActions(checked, authFixturePNG, "account-1")
	if len(actions) != 1 {
		t.Fatalf("checked login method actions = %#v prompt=%q", actions, prompt)
	}
	operation, validID = parseAuthActionID(actions[0].ID)
	if !validID || operation != continueWeComWithWechatAction || actions[0].ReplayKey != continueWeComWithWechatAction || !actions[0].ImageBound ||
		!actions[0].RequiresConfirmation || actions[0].Risk != "high" || prompt == "" {
		t.Fatalf("checked login method actions = %#v prompt=%q", actions, prompt)
	}

	tests := []struct {
		name   string
		mutate func(*UISnapshot)
	}{
		{name: "wrong package", mutate: func(value *UISnapshot) { value.PackageName = "example.invalid" }},
		{name: "wrong activity", mutate: func(value *UISnapshot) { value.WindowClass = weComLaunchActivity }},
		{name: "zero sequence", mutate: func(value *UISnapshot) { value.Sequence = 0 }},
		{name: "missing login method marker", mutate: func(value *UISnapshot) { value.Nodes[1].Text = "" }},
		{name: "hidden login method marker", mutate: func(value *UISnapshot) { value.Nodes[2].VisibleToUser = false }},
		{name: "missing agreement marker", mutate: func(value *UISnapshot) { value.Nodes[4].Text = "" }},
		{name: "hidden agreement marker", mutate: func(value *UISnapshot) { value.Nodes[4].VisibleToUser = false }},
		{name: "partial first-run privacy modal", mutate: func(value *UISnapshot) {
			value.Nodes = append(value.Nodes, Node{ID: "0/5", Text: "Welcome to WeCom!", Enabled: true, VisibleToUser: true})
		}},
		{name: "disabled WeChat button", mutate: func(value *UISnapshot) { value.Nodes[0].Enabled = false }},
		{name: "non-clickable WeChat button", mutate: func(value *UISnapshot) { value.Nodes[0].Clickable = false }},
		{name: "offscreen WeChat button", mutate: func(value *UISnapshot) { value.Nodes[0].Bounds = Bounds{} }},
		{name: "disabled Email button", mutate: func(value *UISnapshot) { value.Nodes[1].Enabled = false }},
		{name: "non-clickable Phone button", mutate: func(value *UISnapshot) { value.Nodes[2].Clickable = false }},
		{name: "offscreen Phone button", mutate: func(value *UISnapshot) { value.Nodes[2].Bounds = Bounds{} }},
		{name: "multiple agreement controls", mutate: func(value *UISnapshot) {
			value.Nodes = append(value.Nodes, Node{
				ID: "0/5", ClassName: androidImageViewClass, ViewID: weComLoginAgreementViewID,
				Clickable: true, Selected: boolPointer(false),
				Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 60, Top: 520, Right: 90, Bottom: 550},
			})
		}},
		{name: "wrong agreement view ID", mutate: func(value *UISnapshot) { value.Nodes[3].ViewID = DefaultWeComPackage + ":id/not-ow" }},
		{name: "wrong agreement class", mutate: func(value *UISnapshot) { value.Nodes[3].ClassName = "android.widget.Button" }},
		{name: "missing selected state", mutate: func(value *UISnapshot) { value.Nodes[3].Selected = nil }},
		{name: "custom control unexpectedly checkable", mutate: func(value *UISnapshot) { value.Nodes[3].Checkable = true }},
		{name: "custom control unexpectedly checked", mutate: func(value *UISnapshot) { value.Nodes[3].Checked = true }},
		{name: "non-clickable agreement control", mutate: func(value *UISnapshot) { value.Nodes[3].Clickable = false }},
		{name: "disabled agreement control", mutate: func(value *UISnapshot) { value.Nodes[3].Enabled = false }},
		{name: "hidden agreement control", mutate: func(value *UISnapshot) { value.Nodes[3].VisibleToUser = false }},
		{name: "offscreen agreement control", mutate: func(value *UISnapshot) { value.Nodes[3].Bounds = Bounds{} }},
		{name: "risk marker", mutate: func(value *UISnapshot) {
			value.Nodes = append(value.Nodes, Node{ID: "0/5", Text: "Device verification", Enabled: true, VisibleToUser: true})
		}},
		{name: "duplicate WeChat button", mutate: func(value *UISnapshot) {
			value.Nodes = append(value.Nodes, Node{
				ID: "0/5", Text: "Continue with WeChat", Clickable: true, Enabled: true,
				VisibleToUser: true, Bounds: Bounds{Left: 20, Top: 540, Right: 340, Bottom: 600},
			})
		}},
		{name: "shared login method target", mutate: func(value *UISnapshot) {
			value.Nodes[0] = Node{ID: "0/5/0", ParentID: "0/5", Text: "Continue with WeChat", Enabled: true, VisibleToUser: true}
			value.Nodes[1] = Node{ID: "0/5/1", ParentID: "0/5", Text: "Continue with Email", Enabled: true, VisibleToUser: true}
			value.Nodes = append(value.Nodes, Node{
				ID: "0/5", Clickable: true, Enabled: true, VisibleToUser: true,
				Bounds: Bounds{Left: 20, Top: 300, Right: 340, Bottom: 430},
			})
		}},
	}
	for _, test := range tests {
		for _, agreementSelected := range []bool{false, true} {
			state := "unchecked"
			if agreementSelected {
				state = "checked"
			}
			t.Run(test.name+"/"+state, func(t *testing.T) {
				value := englishWeComLoginMethodSnapshot(agreementSelected)
				test.mutate(&value)
				if got, _ := authenticationActions(value, authFixturePNG, "account-1"); len(got) != 0 {
					t.Fatalf("unsafe login method screen advertised actions: %#v", got)
				}
			})
		}
	}
}

func TestPerformWeComLoginMethodActionsInOrder(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	var phase atomic.Int32
	var acceptedObservations atomic.Int32
	var dismissedObservations atomic.Int32
	actions := make(chan CompanionAction, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/snapshot":
			currentPhase := phase.Load()
			switch currentPhase {
			case 0:
				_ = json.NewEncoder(writer).Encode(englishWeComLoginMethodSnapshot(false))
			case 1:
				acceptedObservations.Add(1)
				snapshot := englishWeComLoginMethodSnapshot(true)
				snapshot.Sequence = 42
				_ = json.NewEncoder(writer).Encode(snapshot)
			default:
				dismissedObservations.Add(1)
				_ = json.NewEncoder(writer).Encode(UISnapshot{
					Sequence: 43, PackageName: DefaultWeComPackage, WindowClass: weComLoginWxAuthActivity,
					Nodes: []Node{{ID: "0/0", Text: "Waiting for confirmation", Enabled: true, VisibleToUser: true}},
				})
			}
		case request.Method == http.MethodPost && request.URL.Path == "/v1/actions":
			var action CompanionAction
			if err := json.NewDecoder(request.Body).Decode(&action); err != nil {
				http.Error(writer, "bad action", http.StatusBadRequest)
				return
			}
			actions <- action
			currentPhase := phase.Load()
			if currentPhase < 2 {
				phase.Store(currentPhase + 1)
			}
			_ = json.NewEncoder(writer).Encode(ActionResult{Accepted: true, Sequence: int64(42 + currentPhase)})
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
			Executor: authFixtureExecutor(weComLoginWxAuthActivity),
			Verify:   func(context.Context) error { return nil },
		},
	}
	driverInstance := &Driver{runtime: runtime, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	firstSnapshot := englishWeComLoginMethodSnapshot(false)
	firstActionID := bindAuthActionID(acceptWeComLoginTermsAction, firstSnapshot, authFixturePNG, "")
	if err := driverInstance.PerformAuthAction(ctx, core.AuthActionRequest{ActionID: firstActionID, Confirmed: true}); err != nil {
		t.Fatalf("accept login terms: %v", err)
	}
	var first CompanionAction
	select {
	case first = <-actions:
	case <-ctx.Done():
		t.Fatal("accept terms did not reach companion")
	}
	if first.Kind != ActionCheck || first.NodeID != "0/3" || first.ExpectedSequence != 41 || first.Text != "" {
		t.Fatalf("accept terms action = %#v", first)
	}
	secondSnapshot := englishWeComLoginMethodSnapshot(true)
	secondSnapshot.Sequence = 42
	secondActionID := bindAuthActionID(continueWeComWithWechatAction, secondSnapshot, authFixturePNG, "")
	if err := driverInstance.PerformAuthAction(ctx, core.AuthActionRequest{ActionID: secondActionID, Confirmed: true}); err != nil {
		t.Fatalf("continue with WeChat: %v", err)
	}
	var second CompanionAction
	select {
	case second = <-actions:
	case <-ctx.Done():
		t.Fatal("continue with WeChat did not reach companion")
	}
	if second.Kind != ActionClick || second.NodeID != "0/0" || second.ExpectedSequence != 42 || second.Text != "" {
		t.Fatalf("continue with WeChat action = %#v", second)
	}
	if acceptedObservations.Load() < 2 {
		t.Fatalf("terms action returned before stable accepted state: observations=%d", acceptedObservations.Load())
	}
	if dismissedObservations.Load() < 2 {
		t.Fatalf("continue action returned before stable marker dismissal: observations=%d", dismissedObservations.Load())
	}
}

func TestPerformAuthActionsConsumeUncertainCompanionDispatch(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	privacy := UISnapshot{
		Sequence: 23, PackageName: DefaultWeComPackage, WindowClass: weComLoginWxAuthActivity,
		Nodes: []Node{
			{ID: "0/0", Text: "Privacy Policy", Enabled: true, VisibleToUser: true},
			{ID: "0/1", Text: "Welcome to WeCom!", Enabled: true, VisibleToUser: true},
			{ID: "0/2", Text: "Agree", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 10, Top: 10, Right: 100, Bottom: 50}},
			{ID: "0/3", Text: "Disagree", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 10, Top: 60, Right: 100, Bottom: 100}},
		},
	}
	tests := []struct {
		name       string
		operation  string
		snapshot   UISnapshot
		wantKind   string
		wantNodeID string
	}{
		{name: "privacy consent", operation: acceptPrivacyPolicyAction, snapshot: privacy, wantKind: ActionClick, wantNodeID: "0/2"},
		{name: "login terms", operation: acceptWeComLoginTermsAction, snapshot: englishWeComLoginMethodSnapshot(false), wantKind: ActionCheck, wantNodeID: "0/3"},
		{name: "continue with WeChat", operation: continueWeComWithWechatAction, snapshot: englishWeComLoginMethodSnapshot(true), wantKind: ActionClick, wantNodeID: "0/0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			received := make(chan CompanionAction, 1)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch {
				case request.Method == http.MethodGet && request.URL.Path == "/v1/snapshot":
					writer.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(writer).Encode(test.snapshot)
				case request.Method == http.MethodPost && request.URL.Path == "/v1/actions":
					var action CompanionAction
					if err := json.NewDecoder(request.Body).Decode(&action); err != nil {
						http.Error(writer, "bad action", http.StatusBadRequest)
						return
					}
					received <- action
					connection, _, err := writer.(http.Hijacker).Hijack()
					if err != nil {
						t.Errorf("hijack action response: %v", err)
						return
					}
					_ = connection.Close()
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
					Executor: authFixtureExecutor(weComLoginWxAuthActivity),
					Verify:   func(context.Context) error { return nil },
				},
			}
			driverInstance := &Driver{
				runtime: runtime, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo),
			}
			err = driverInstance.PerformAuthAction(context.Background(), core.AuthActionRequest{
				ActionID: bindAuthActionID(test.operation, test.snapshot, authFixturePNG, ""), Confirmed: true,
			})
			if !errors.Is(err, ErrActionOutcomeUncertain) {
				t.Fatalf("action error = %v, want ErrActionOutcomeUncertain", err)
			}
			if !core.AuthActionWasConsumed(err) {
				t.Fatalf("uncertain dispatched action was not marked consumed: %v", err)
			}
			select {
			case action := <-received:
				if action.Kind != test.wantKind || action.NodeID != test.wantNodeID || action.ExpectedSequence != test.snapshot.Sequence {
					t.Fatalf("companion action = %#v", action)
				}
			default:
				t.Fatal("companion handler did not receive dispatched action")
			}
		})
	}
}

func TestPerformAuthActionExplicitCompanionRejectionIsNotConsumed(t *testing.T) {
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
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/snapshot":
			_ = json.NewEncoder(writer).Encode(snapshot)
		case request.Method == http.MethodPost && request.URL.Path == "/v1/actions":
			_ = json.NewEncoder(writer).Encode(ActionResult{Accepted: false, Sequence: snapshot.Sequence, Detail: "stale node"})
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
			Executor: authFixtureExecutor(weComLoginWxAuthActivity),
			Verify:   func(context.Context) error { return nil },
		},
	}
	driverInstance := &Driver{runtime: runtime, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}
	err = driverInstance.PerformAuthAction(context.Background(), core.AuthActionRequest{
		ActionID: bindAuthActionID(acceptPrivacyPolicyAction, snapshot, authFixturePNG, ""), Confirmed: true,
	})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("action error = %v, want ErrStale", err)
	}
	if errors.Is(err, ErrActionOutcomeUncertain) || core.AuthActionWasConsumed(err) {
		t.Fatalf("explicit companion rejection was treated as dispatched: %v", err)
	}
}

func TestPerformContinueWithWeChatConsumesAcceptedActionBeforeHardRiskHandoff(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	snapshot := englishWeComLoginMethodSnapshot(true)
	var acted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/snapshot":
			if !acted.Load() {
				_ = json.NewEncoder(writer).Encode(snapshot)
				return
			}
			_ = json.NewEncoder(writer).Encode(UISnapshot{
				Sequence: snapshot.Sequence + 1, PackageName: DefaultWeComPackage,
				WindowClass: weComLoginWxAuthActivity,
				Nodes:       []Node{{ID: "0/0", Text: "Device verification", VisibleToUser: true}},
			})
		case request.Method == http.MethodPost && request.URL.Path == "/v1/actions":
			acted.Store(true)
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
	runtime := &Runtime{
		config: DefaultConfig(), companion: client, running: true,
		android: AndroidContainer{
			DockerBinary: "docker", Container: "synthetic",
			Executor: authFixtureExecutor(weComLoginWxAuthActivity),
			Verify:   func(context.Context) error { return nil },
		},
	}
	driverInstance := &Driver{runtime: runtime, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}
	err = driverInstance.PerformAuthAction(context.Background(), core.AuthActionRequest{
		ActionID: bindAuthActionID(continueWeComWithWechatAction, snapshot, authFixturePNG, ""), Confirmed: true,
	})
	if !errors.Is(err, ErrUserActionRequired) {
		t.Fatalf("hard-risk handoff error = %v, want ErrUserActionRequired", err)
	}
	if !core.AuthActionWasConsumed(err) {
		t.Fatalf("accepted action was retryable after hard-risk handoff: %v", err)
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
			Executor: authFixtureExecutor(weComLoginWxAuthActivity),
			Verify:   func(context.Context) error { return nil },
		},
	}
	driverInstance := &Driver{runtime: runtime, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}
	actionID := bindAuthActionID(acceptPrivacyPolicyAction, snapshot, authFixturePNG, "")
	if err := driverInstance.PerformAuthAction(context.Background(), core.AuthActionRequest{ActionID: actionID}); !errors.Is(err, ErrUserActionRequired) {
		t.Fatalf("unconfirmed consent error = %v", err)
	}
	select {
	case action := <-actions:
		t.Fatalf("unconfirmed consent reached companion: %#v", action)
	default:
	}
	if err := driverInstance.PerformAuthAction(context.Background(), core.AuthActionRequest{ActionID: actionID, Confirmed: true}); err != nil {
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

func TestHighRiskSnapshotIsRejectedBeforeScreenshot(t *testing.T) {
	executor := &recordingExecutor{output: resumedActivityOutput("com.tencent.wework.pay.controller.PaymentActivity")}
	runtime := &Runtime{
		config:  DefaultConfig(),
		running: true,
		android: AndroidContainer{
			DockerBinary: "docker", Container: "synthetic", Executor: executor,
			Verify: func(context.Context) error { return nil },
		},
	}
	driver := &Driver{runtime: runtime, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}
	_, err := driver.updateSurface(context.Background(), "surface-risk", UISnapshot{
		Sequence: 9, PackageName: DefaultWeComPackage, WindowID: 7, WindowTitle: "订单支付",
		Nodes: []Node{
			{ID: "0/confirm", Text: "确认支付", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 20, Top: 100, Right: 200, Bottom: 160}},
			{ID: "0/note", Text: "备注", Editable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 20, Top: 180, Right: 300, Bottom: 240}},
			{ID: "0/list", Text: "详情", Scrollable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 20, Top: 260, Right: 300, Bottom: 500}},
		},
	}, surfaceIdentity{})
	if !errors.Is(err, ErrUserActionRequired) {
		t.Fatalf("high-risk surface error = %v", err)
	}
	if executorCalledScreenshot(executor) || len(driver.surfaces) != 0 {
		t.Fatalf("high-risk surface was captured or recorded: calls=%#v surfaces=%#v", executor.calls, driver.surfaces)
	}
}

func TestAgreementImageCannotProduceAGenericSurface(t *testing.T) {
	executor := &recordingExecutor{output: resumedActivityOutput("com.tencent.wework.msg.controller.ConversationActivity")}
	runtime := &Runtime{
		config:  DefaultConfig(),
		running: true,
		android: AndroidContainer{
			DockerBinary: "docker", Container: "synthetic", Executor: executor,
			Verify: func(context.Context) error { return nil },
		},
	}
	driver := &Driver{runtime: runtime, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}
	_, err := driver.updateSurface(context.Background(), "surface-agreement", UISnapshot{
		Sequence: 9, PackageName: DefaultWeComPackage, WindowID: 7,
		Nodes: []Node{{
			ID: "0/agreement", ClassName: androidImageViewClass, ViewID: weComLoginAgreementViewID,
			Clickable: true, Enabled: true, VisibleToUser: true,
		}},
	}, surfaceIdentity{})
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("agreement surface error = %v", err)
	}
	if executorCalledScreenshot(executor) || len(driver.surfaces) != 0 {
		t.Fatalf("agreement surface was captured or recorded: calls=%#v surfaces=%#v", executor.calls, driver.surfaces)
	}
}

func TestAuthenticationScreenIsRejectedBeforeScreenshot(t *testing.T) {
	executor := &recordingExecutor{output: resumedActivityOutput(weComLoginWxAuthActivity)}
	runtime := &Runtime{
		config:  DefaultConfig(),
		running: true,
		android: AndroidContainer{
			DockerBinary: "docker", Container: "synthetic", Executor: executor,
			Verify: func(context.Context) error { return nil },
		},
	}
	driver := &Driver{runtime: runtime, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}
	snapshot := englishWeComLoginMethodSnapshot(false)
	snapshot.WindowID = 7
	snapshot.WindowClass = ""
	snapshot.Nodes = append(snapshot.Nodes, Node{
		ID: "0/input", ClassName: "android.widget.EditText", Editable: true,
		Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 20, Top: 520, Right: 340, Bottom: 580},
	})
	_, err := driver.updateSurface(context.Background(), "surface-auth", snapshot, surfaceIdentity{})
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("authentication surface error = %v", err)
	}
	if executorCalledScreenshot(executor) || len(driver.surfaces) != 0 {
		t.Fatalf("authentication surface was captured or recorded: calls=%#v surfaces=%#v", executor.calls, driver.surfaces)
	}
}

func TestAuthenticationSurfaceFallbackMarkersFailClosed(t *testing.T) {
	login := englishWeComLoginMethodSnapshot(false)
	login.WindowClass = ""
	tests := []struct {
		name     string
		snapshot UISnapshot
		want     bool
	}{
		{name: "login methods without activity", snapshot: login, want: true},
		{name: "exact agreement control without activity or state", snapshot: UISnapshot{
			PackageName: DefaultWeComPackage,
			Nodes: []Node{{
				ID: "0/agreement", ClassName: androidImageViewClass, ViewID: weComLoginAgreementViewID,
			}},
		}, want: true},
		{name: "structured Chinese privacy guide without activity", snapshot: UISnapshot{
			PackageName: DefaultWeComPackage,
			Nodes: []Node{
				{ID: "0/privacy", Text: "欢迎使用企业微信，请阅读隐私政策", VisibleToUser: true},
				{ID: "0/agree", Text: "同意", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 20, Top: 100, Right: 120, Bottom: 150}},
				{ID: "0/disagree", Text: "不同意", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 140, Top: 100, Right: 240, Bottom: 150}},
			},
		}, want: true},
		{name: "structured SMS without activity", snapshot: UISnapshot{
			PackageName: DefaultWeComPackage,
			Nodes: []Node{
				{ID: "0/label", Text: "Enter verification code", VisibleToUser: true},
				{ID: "0/input", Editable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 20, Top: 100, Right: 300, Bottom: 160}},
				{ID: "0/submit", Text: "Continue", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 20, Top: 180, Right: 300, Bottom: 240}},
			},
		}, want: true},
		{name: "structured QR without activity", snapshot: UISnapshot{
			PackageName: DefaultWeComPackage,
			Nodes: []Node{
				{ID: "0/label", Text: "Scan with WeChat", VisibleToUser: true, Bounds: Bounds{Left: 20, Top: 20, Right: 300, Bottom: 70}},
				{ID: "0/qr", ClassName: "android.widget.ImageView", VisibleToUser: true, Bounds: Bounds{Left: 60, Top: 100, Right: 300, Bottom: 340}},
			},
		}, want: true},
		{name: "benign official surface", snapshot: UISnapshot{
			PackageName: DefaultWeComPackage,
			Nodes:       []Node{{ID: "0/label", Text: "Project updates", VisibleToUser: true}},
		}, want: false},
		{name: "lookalike login activity", snapshot: UISnapshot{
			PackageName: DefaultWeComPackage,
			WindowClass: weComLoginWxAuthActivity + "Evil",
		}, want: false},
		{name: "foreign marker", snapshot: UISnapshot{
			PackageName: "example.invalid",
			Nodes:       []Node{{ID: "0/label", Text: "Scan with WeChat", VisibleToUser: true}},
		}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := authenticationSurface(test.snapshot); got != test.want {
				t.Fatalf("authenticationSurface() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestOpenSurfaceRejectsGenericUILabelReference(t *testing.T) {
	driver := &Driver{runtime: &Runtime{}, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo)}
	_, err := driver.OpenSurface(context.Background(), "打开任意当前按钮")
	if !errors.Is(err, ErrUnsupportedCapability) {
		t.Fatalf("expected generic label reference to be rejected, got %v", err)
	}
}

func resumedActivityOutput(activity string) []byte {
	return []byte("topResumedActivity=ActivityRecord{x u0 " + DefaultWeComPackage + "/" + activity + "}\n")
}

func authFixtureExecutor(activity string) Executor {
	return functionExecutor(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "/system/bin/screencap") {
			return append([]byte(nil), authFixturePNG...), nil
		}
		return resumedActivityOutput(activity), nil
	})
}

func executorCalledScreenshot(executor *recordingExecutor) bool {
	for _, call := range executor.calls {
		if strings.Contains(strings.Join(call.args, " "), "/system/bin/screencap") {
			return true
		}
	}
	return false
}
