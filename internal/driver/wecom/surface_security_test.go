package wecom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	core "github.com/gih10012/wechatcopilot/internal/driver"
)

const testConversationActivity = "com.tencent.wework.msg.controller.ConversationActivity"

func TestSnapshotSurfaceRejectsUnsafeOrChangedWindowBeforeScreenshot(t *testing.T) {
	bound := testSurfaceSnapshot(10, 7, testConversationActivity)
	tests := []struct {
		name     string
		current  UISnapshot
		activity string
		want     error
	}{
		{
			name: "login QR",
			current: UISnapshot{
				Sequence: 11, PackageName: DefaultWeComPackage, WindowID: 8,
				Nodes: []Node{{ID: "0/qr", Text: "Scan with WeChat", VisibleToUser: true}},
			},
			activity: weComLoginWxAuthActivity,
			want:     ErrAuthRequired,
		},
		{
			name: "SMS verification",
			current: UISnapshot{
				Sequence: 11, PackageName: DefaultWeComPackage, WindowID: 8,
				Nodes: []Node{{ID: "0/code", Text: "验证码", VisibleToUser: true}},
			},
			activity: weComSMSVerifyActivity,
			want:     ErrAuthRequired,
		},
		{
			name: "foreign package",
			current: UISnapshot{
				Sequence: 11, PackageName: "com.android.systemui", WindowID: 8,
			},
			activity: testConversationActivity,
			want:     ErrTargetAmbiguous,
		},
		{
			name:     "window ID changed",
			current:  testSurfaceSnapshot(11, 8, testConversationActivity),
			activity: testConversationActivity,
			want:     ErrStale,
		},
		{
			name:     "activity changed",
			current:  testSurfaceSnapshot(11, 7, "com.tencent.wework.contacts.controller.ContactActivity"),
			activity: "com.tencent.wework.contacts.controller.ContactActivity",
			want:     ErrStale,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			driver, transport := newSurfaceTestDriver(t, test.current, test.activity)
			driver.surfaces["surface-1"] = testSurfaceState(bound)

			_, err := driver.SnapshotSurface(context.Background(), "surface-1")
			if !errors.Is(err, test.want) {
				t.Fatalf("SnapshotSurface() error = %v, want %v", err, test.want)
			}
			if transport.screenshotCount() != 0 {
				t.Fatalf("unsafe snapshot invoked screencap %d times", transport.screenshotCount())
			}
		})
	}
}

func TestSnapshotSurfaceRefreshesOnlyTheBoundWindow(t *testing.T) {
	bound := testSurfaceSnapshot(10, 7, testConversationActivity)
	current := testSurfaceSnapshot(11, 7, testConversationActivity)
	driver, transport := newSurfaceTestDriver(t, current, testConversationActivity)
	driver.surfaces["surface-1"] = testSurfaceState(bound)

	surface, err := driver.SnapshotSurface(context.Background(), "surface-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(surface.Screenshot) < 8 || transport.screenshotCount() != 1 {
		t.Fatalf("bound surface screenshot was not captured exactly once: surface=%#v count=%d", surface, transport.screenshotCount())
	}
	if state := driver.surfaces["surface-1"]; state.sequence != current.Sequence || state.identity != identityForSurface(current) {
		t.Fatalf("surface state was not refreshed in place: %#v", state)
	}
}

func TestSnapshotSurfaceDiscardsScreenshotWhenWindowChangesDuringCapture(t *testing.T) {
	bound := testSurfaceSnapshot(10, 7, testConversationActivity)
	tests := []struct {
		name       string
		transition UISnapshot
		activity   string
		want       error
	}{
		{
			name: "login",
			transition: UISnapshot{
				Sequence: 11, PackageName: DefaultWeComPackage, WindowID: 8,
				Nodes: []Node{{ID: "0/qr", Text: "Scan with WeChat", VisibleToUser: true}},
			},
			activity: weComLoginWxAuthActivity,
			want:     ErrAuthRequired,
		},
		{
			name:       "foreign package",
			transition: UISnapshot{Sequence: 11, PackageName: "com.android.systemui", WindowID: 8},
			activity:   testConversationActivity,
			want:       ErrTargetAmbiguous,
		},
		{
			name:       "same window sequence drift",
			transition: testSurfaceSnapshot(11, 7, testConversationActivity),
			activity:   testConversationActivity,
			want:       ErrStale,
		},
		{
			name:       "risk verification",
			transition: structuredRiskOverlaySnapshot(11, 7),
			activity:   testConversationActivity,
			want:       ErrUserActionRequired,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			driver, transport := newSurfaceTestDriver(t, bound, testConversationActivity)
			driver.surfaces["surface-1"] = testSurfaceState(bound)
			transport.screenshotTransition = &test.transition
			transport.screenshotActivity = test.activity

			surface, err := driver.SnapshotSurface(context.Background(), "surface-1")
			if !errors.Is(err, test.want) {
				t.Fatalf("SnapshotSurface() error = %v, want %v", err, test.want)
			}
			if len(surface.Screenshot) != 0 || transport.screenshotCount() != 1 {
				t.Fatalf("raced screenshot escaped: bytes=%d count=%d", len(surface.Screenshot), transport.screenshotCount())
			}
			if state := driver.surfaces["surface-1"]; state.sequence != bound.Sequence || state.identity != identityForSurface(bound) {
				t.Fatalf("rejected screenshot changed surface state: %#v", state)
			}
		})
	}
}

func TestSnapshotSurfaceProbeFailureStopsBeforeScreenshot(t *testing.T) {
	current := testSurfaceSnapshot(10, 7, testConversationActivity)
	driver, transport := newSurfaceTestDriver(t, current, testConversationActivity)
	driver.surfaces["surface-1"] = testSurfaceState(current)
	transport.activityErr = errors.New("foreground probe failed")

	_, err := driver.SnapshotSurface(context.Background(), "surface-1")
	if !errors.Is(err, ErrStale) {
		t.Fatalf("probe failure error = %v, want ErrStale", err)
	}
	if transport.screenshotCount() != 0 {
		t.Fatalf("probe failure invoked screencap %d times", transport.screenshotCount())
	}
}

func TestAuthSnapshotCapturesOnlyAStableExactAuthenticationWindow(t *testing.T) {
	tests := []struct {
		name       string
		current    UISnapshot
		activity   string
		probeError bool
	}{
		{
			name:     "foreign package",
			current:  UISnapshot{Sequence: 10, PackageName: "com.android.systemui", WindowID: 7},
			activity: testConversationActivity,
		},
		{
			name:     "ordinary official page",
			current:  testSurfaceSnapshot(10, 7, testConversationActivity),
			activity: testConversationActivity,
		},
		{
			name: "authentication marker in ordinary activity",
			current: UISnapshot{
				Sequence: 10, PackageName: DefaultWeComPackage, WindowID: 7,
				Nodes: []Node{{ID: "0/qr", Text: "Scan with WeChat", VisibleToUser: true}},
			},
			activity: testConversationActivity,
		},
		{
			name: "foreground probe failure",
			current: UISnapshot{
				Sequence: 10, PackageName: DefaultWeComPackage, WindowID: 7,
				Nodes: []Node{{ID: "0/qr", Text: "Scan with WeChat", VisibleToUser: true}},
			},
			activity:   weComLoginWxAuthActivity,
			probeError: true,
		},
		{
			name: "missing window ID",
			current: UISnapshot{
				Sequence: 10, PackageName: DefaultWeComPackage, WindowID: -1,
				Nodes: []Node{{ID: "0/qr", Text: "Scan with WeChat", VisibleToUser: true}},
			},
			activity: weComLoginWxAuthActivity,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			driver, transport := newSurfaceTestDriver(t, test.current, test.activity)
			if test.probeError {
				transport.activityErr = errors.New("foreground probe failed")
			}
			snapshot, err := driver.AuthSnapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.ScreenshotPNG) != 0 || len(snapshot.QRCodePNG) != 0 || len(snapshot.Actions) != 0 ||
				snapshot.CanSubmitCode || transport.screenshotCount() != 0 {
				t.Fatalf("non-auth window was captured: snapshot=%#v count=%d", snapshot, transport.screenshotCount())
			}
		})
	}

	auth := UISnapshot{
		Sequence: 10, PackageName: DefaultWeComPackage, WindowID: 7,
		Nodes: []Node{{ID: "0/qr", Text: "Scan with WeChat", VisibleToUser: true}},
	}
	driver, transport := newSurfaceTestDriver(t, auth, weComLoginWxAuthActivity)
	snapshot, err := driver.AuthSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ScreenshotPNG) < 8 || len(snapshot.QRCodePNG) < 8 || transport.screenshotCount() != 1 {
		t.Fatalf("stable authentication window was not captured: snapshot=%#v count=%d", snapshot, transport.screenshotCount())
	}
}

func TestAuthSnapshotDiscardsCaptureThatTransitionsToForeignUI(t *testing.T) {
	auth := UISnapshot{
		Sequence: 10, PackageName: DefaultWeComPackage, WindowID: 7,
		Nodes: []Node{{ID: "0/qr", Text: "Scan with WeChat", VisibleToUser: true}},
	}
	driver, transport := newSurfaceTestDriver(t, auth, weComLoginWxAuthActivity)
	foreign := UISnapshot{Sequence: 11, PackageName: "com.android.systemui", WindowID: 8}
	transport.screenshotTransition = &foreign
	transport.screenshotActivity = testConversationActivity

	snapshot, err := driver.AuthSnapshot(context.Background())
	if !errors.Is(err, ErrTargetAmbiguous) {
		t.Fatalf("raced authentication capture error = %v, want ErrTargetAmbiguous", err)
	}
	if len(snapshot.ScreenshotPNG) != 0 || len(snapshot.QRCodePNG) != 0 || transport.screenshotCount() != 1 {
		t.Fatalf("raced authentication screenshot escaped: snapshot=%#v count=%d", snapshot, transport.screenshotCount())
	}
}

func TestAuthSnapshotRejectsSameSequencePolicyTransitionDuringCapture(t *testing.T) {
	privacy := testPrivacyAuthSnapshot(31, 7)
	driver, transport := newSurfaceTestDriver(t, privacy, weComLoginWxAuthActivity)
	loginMethods := englishWeComLoginMethodSnapshot(false)
	loginMethods.Sequence = privacy.Sequence
	loginMethods.WindowID = privacy.WindowID
	transport.screenshotTransition = &loginMethods
	transport.screenshotActivity = weComLoginWxAuthActivity

	snapshot, err := driver.AuthSnapshot(context.Background())
	if !errors.Is(err, ErrStale) {
		t.Fatalf("same-sequence policy transition error = %v, want ErrStale", err)
	}
	if len(snapshot.ScreenshotPNG) != 0 || len(snapshot.Actions) != 0 || transport.screenshotCount() != 1 {
		t.Fatalf("policy-raced authentication state escaped: snapshot=%#v screenshots=%d", snapshot, transport.screenshotCount())
	}
}

func TestWeComAuthActionGenerationBindsSnapshotAccountAndScreenshot(t *testing.T) {
	base := testPrivacyAuthSnapshot(41, 7)
	driver, transport := newSurfaceTestDriver(t, base, weComLoginWxAuthActivity)
	driver.account = core.AccountRuntime{AccountID: "account-a"}
	transport.setScreenshotPNG(authFixturePNG)

	first, err := driver.AuthSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstID := requireSingleImageBoundAuthAction(t, first).ID
	if !strings.HasPrefix(firstID, acceptPrivacyPolicyAction+".") || strings.Contains(firstID, "account-a") ||
		!bytes.Equal(first.ScreenshotPNG, authFixturePNG) {
		t.Fatalf("first authentication binding is not opaque or image-bound: id=%q screenshot=%q", firstID, first.ScreenshotPNG)
	}

	tests := []struct {
		name  string
		apply func()
		reset func()
	}{
		{name: "sequence", apply: func() {
			changed := base
			changed.Sequence++
			transport.setSnapshot(changed, weComLoginWxAuthActivity)
		}, reset: func() { transport.setSnapshot(base, weComLoginWxAuthActivity) }},
		{name: "window identity", apply: func() {
			changed := base
			changed.WindowID++
			transport.setSnapshot(changed, weComLoginWxAuthActivity)
		}, reset: func() { transport.setSnapshot(base, weComLoginWxAuthActivity) }},
		{name: "activity", apply: func() {
			transport.setSnapshot(base, weComLaunchActivity)
		}, reset: func() { transport.setSnapshot(base, weComLoginWxAuthActivity) }},
		{name: "account", apply: func() {
			driver.account = core.AccountRuntime{AccountID: "account-b"}
		}, reset: func() { driver.account = core.AccountRuntime{AccountID: "account-a"} }},
		{name: "screenshot", apply: func() {
			transport.setScreenshotPNG([]byte("\x89PNG\r\n\x1a\nchanged-auth-frame"))
		}, reset: func() { transport.setScreenshotPNG(authFixturePNG) }},
		{name: "policy", apply: func() {
			changed := englishWeComLoginMethodSnapshot(false)
			changed.Sequence = base.Sequence
			changed.WindowID = base.WindowID
			transport.setSnapshot(changed, weComLoginWxAuthActivity)
		}, reset: func() { transport.setSnapshot(base, weComLoginWxAuthActivity) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.apply()
			defer test.reset()
			current, snapshotErr := driver.AuthSnapshot(context.Background())
			if snapshotErr != nil {
				t.Fatal(snapshotErr)
			}
			currentAction := requireSingleImageBoundAuthAction(t, current)
			if currentAction.ID == firstID {
				t.Fatalf("%s change retained stale generation %q", test.name, firstID)
			}
		})
	}
}

func TestPerformWeComAuthActionRejectsEveryStaleGenerationBeforeDispatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Driver, *surfaceTestTransport, UISnapshot)
	}{
		{name: "policy A image then policy B", mutate: func(_ *Driver, transport *surfaceTestTransport, base UISnapshot) {
			changed := englishWeComLoginMethodSnapshot(false)
			changed.Sequence, changed.WindowID = base.Sequence, base.WindowID
			transport.setSnapshot(changed, weComLoginWxAuthActivity)
		}},
		{name: "sequence", mutate: func(_ *Driver, transport *surfaceTestTransport, base UISnapshot) {
			base.Sequence++
			transport.setSnapshot(base, weComLoginWxAuthActivity)
		}},
		{name: "window identity", mutate: func(_ *Driver, transport *surfaceTestTransport, base UISnapshot) {
			base.WindowID++
			transport.setSnapshot(base, weComLoginWxAuthActivity)
		}},
		{name: "activity", mutate: func(_ *Driver, transport *surfaceTestTransport, base UISnapshot) {
			transport.setSnapshot(base, weComLaunchActivity)
		}},
		{name: "account", mutate: func(driver *Driver, _ *surfaceTestTransport, _ UISnapshot) {
			driver.account = core.AccountRuntime{AccountID: "account-b"}
		}},
		{name: "screenshot", mutate: func(_ *Driver, transport *surfaceTestTransport, _ UISnapshot) {
			transport.setScreenshotPNG([]byte("\x89PNG\r\n\x1a\nnew-policy-image"))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := testPrivacyAuthSnapshot(51, 9)
			driver, transport := newSurfaceTestDriver(t, base, weComLoginWxAuthActivity)
			driver.account = core.AccountRuntime{AccountID: "account-a"}
			transport.setScreenshotPNG(authFixturePNG)
			advertised, err := driver.AuthSnapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			staleID := requireSingleImageBoundAuthAction(t, advertised).ID
			test.mutate(driver, transport, base)
			err = driver.PerformAuthAction(context.Background(), core.AuthActionRequest{ActionID: staleID, Confirmed: true})
			if !errors.Is(err, ErrStale) {
				t.Fatalf("stale action error = %v, want ErrStale", err)
			}
			if actions := transport.recordedActions(); len(actions) != 0 {
				t.Fatalf("stale action reached companion: %#v", actions)
			}
		})
	}
}

func TestExactImageBoundAuthActionRejectsDuplicates(t *testing.T) {
	action := core.AuthAction{ID: acceptPrivacyPolicyAction + "." + strings.Repeat("a", 64), ImageBound: true}
	snapshot := core.AuthSnapshot{State: core.StateAuthRequired, Actions: []core.AuthAction{action, action}}
	if exactImageBoundAuthAction(snapshot, action.ID) {
		t.Fatal("duplicate authentication actions were accepted as one generation")
	}
}

func TestActSurfaceNeverRebindsAnUnprovenWindowTransition(t *testing.T) {
	bound := testSurfaceSnapshot(10, 7, testConversationActivity)
	transition := testSurfaceSnapshot(11, 8, "com.tencent.wework.contacts.controller.ContactActivity")
	tests := []struct {
		name   string
		kind   string
		input  core.SurfaceAction
		nodeID string
	}{
		{name: "set text", kind: ActionSetText, input: core.SurfaceAction{ActionID: "action-1", Text: "draft", TextProvided: true}, nodeID: "0/input"},
		{name: "scroll", kind: ActionScrollForward, input: core.SurfaceAction{ActionID: "action-1"}, nodeID: "0/list"},
		{name: "click", kind: ActionClick, input: core.SurfaceAction{ActionID: "action-1"}, nodeID: "0/button"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			current := bound
			current.Nodes = append([]Node(nil), bound.Nodes...)
			switch test.kind {
			case ActionSetText:
				current.Nodes = append(current.Nodes, Node{
					ID: test.nodeID, Editable: true, Enabled: true, VisibleToUser: true,
					Bounds: Bounds{Left: 1, Top: 80, Right: 80, Bottom: 100},
				})
			case ActionClick:
				current.Nodes = append(current.Nodes, Node{
					ID: test.nodeID, Text: "Unknown", Clickable: true, Enabled: true, VisibleToUser: true,
					Bounds: Bounds{Left: 80, Top: 80, Right: 100, Bottom: 100},
				})
			}
			driver, transport := newSurfaceTestDriver(t, current, testConversationActivity)
			state := testSurfaceState(current)
			node, err := uniqueNode(current, func(node Node) bool { return node.ID == test.nodeID })
			if err != nil {
				t.Fatal(err)
			}
			label := surfaceActionLabel(current, node, test.kind)
			risk, effect := classifySurfaceAction(current, node, test.kind)
			boundAction := bindSurfaceAction(
				"surface-1", current, node, test.kind, label, risk, effect, surfaceContextDigest(current),
			)
			state.actions[boundAction.advertised.ID] = boundAction
			driver.surfaces["surface-1"] = state
			transport.actionTransition = &transition
			transport.actionActivity = transition.WindowClass
			test.input.ActionID = boundAction.advertised.ID
			if risk != "low" || effect == "external_write" {
				test.input.Confirmed = true
			}

			surface, err := driver.ActSurface(context.Background(), "surface-1", test.input)
			if !errors.Is(err, ErrStale) {
				t.Fatalf("ActSurface() error = %v, want ErrStale", err)
			}
			if len(surface.Screenshot) != 0 || transport.screenshotCount() != 0 {
				t.Fatalf("unproven transition was captured: bytes=%d count=%d", len(surface.Screenshot), transport.screenshotCount())
			}
			if actions := transport.recordedActions(); len(actions) != 1 || actions[0].Kind != test.kind {
				t.Fatalf("unexpected companion actions: %#v", actions)
			}
		})
	}
}

func TestActSurfaceRequiresExplicitTextPresenceAndAllowsExplicitEmpty(t *testing.T) {
	current := testSurfaceSnapshot(10, 7, testConversationActivity)
	current.Nodes = append(current.Nodes, Node{
		ID: "0/input", ClassName: "android.widget.EditText", Editable: true, Enabled: true,
		VisibleToUser: true, Bounds: Bounds{Left: 10, Top: 110, Right: 90, Bottom: 140},
	})
	driver, transport := newSurfaceTestDriver(t, current, testConversationActivity)
	driver.surfaces["surface-1"] = testSurfaceState(current)
	surface, err := driver.SnapshotSurface(context.Background(), "surface-1")
	if err != nil {
		t.Fatal(err)
	}
	input := requireSurfaceAction(t, surface, "android.widget.EditText", ActionSetText)

	for _, action := range []core.SurfaceAction{
		{ActionID: input.ID, Confirmed: true},
		{ActionID: input.ID, Text: "hidden", Confirmed: true},
		{ActionID: input.ID, Text: strings.Repeat("界", 4_097), TextProvided: true, Confirmed: true},
		{ActionID: input.ID, Text: "bad\x00text", TextProvided: true, Confirmed: true},
	} {
		if _, err := driver.ActSurface(context.Background(), "surface-1", action); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("invalid text argument error = %v, want ErrInvalidArgument", err)
		} else if kind, ok := core.ClassifyFailure(err); !ok || kind != core.FailureInvalidArgument {
			t.Fatalf("invalid text failure classification = %q, %v; want %q", kind, ok, core.FailureInvalidArgument)
		}
	}
	if actions := transport.recordedActions(); len(actions) != 0 {
		t.Fatalf("inconsistent text presence reached companion: %#v", actions)
	}

	after := current
	after.Sequence = 11
	transport.actionTransition = &after
	transport.actionActivity = testConversationActivity
	updated, err := driver.ActSurface(context.Background(), "surface-1", core.SurfaceAction{
		ActionID: input.ID, TextProvided: true, Confirmed: true,
	})
	if err != nil {
		t.Fatalf("explicit empty set-text failed: %v", err)
	}
	actions := transport.recordedActions()
	if len(actions) != 1 || actions[0].Kind != ActionSetText || actions[0].Text != "" ||
		actions[0].ExpectedSequence != current.Sequence {
		t.Fatalf("explicit empty companion action = %#v", actions)
	}

	scroll := requireSurfaceAction(t, updated, "Scroll forward: Project updates", ActionScrollForward)
	if _, err := driver.ActSurface(context.Background(), "surface-1", core.SurfaceAction{
		ActionID: scroll.ID, TextProvided: true,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("non-input explicit text error = %v, want ErrInvalidArgument", err)
	}
	if got := len(transport.recordedActions()); got != 1 {
		t.Fatalf("non-input explicit text reached companion: actions=%d", got)
	}
}

func TestSurfaceSafetyClassificationRejectsDestructiveAndInvisibleMarkerBypasses(t *testing.T) {
	snapshot := testSurfaceSnapshot(10, 7, testConversationActivity)
	for _, label := range []string{
		"Delete", "Remove", "删除", "清空数据", "Erase data", "Clear history",
		"D\u200belete", "删\u200b除", "P\u200bay", "支\u200b付", "Auth\u200borize",
		"D\u034felete", "D\ufe0felete", "删\u034f除", "P\ufe0fay",
		"Ｄｅｌｅｔｅ",
	} {
		t.Run(label, func(t *testing.T) {
			node := Node{
				ID: "0/action", Text: label, Clickable: true, Enabled: true,
				VisibleToUser: true, Bounds: Bounds{Left: 10, Top: 10, Right: 100, Bottom: 40},
			}
			risk, effect := classifySurfaceAction(snapshot, node, ActionClick)
			if risk != "high" || effect != "high_risk" {
				t.Fatalf("classification for %q = %q/%q", label, risk, effect)
			}
			bound := bindSurfaceAction(
				"surface-1", snapshot, node, ActionClick, label, risk, effect,
				surfaceContextDigest(snapshot),
			)
			if !bound.advertised.Disabled {
				t.Fatalf("destructive action %q was not permanently disabled", label)
			}
		})
	}
	for _, label := range []string{"Information", "Clear search", "Removeable details"} {
		if destructiveSurfaceLabel(label, "") {
			t.Fatalf("ordinary label %q was treated as destructive", label)
		}
	}
}

func TestSurfaceSafetyUsesDescriptionAndBoundedChildEvidence(t *testing.T) {
	snapshot := testSurfaceSnapshot(10, 7, testConversationActivity)
	described := Node{
		ID: "0/described", Text: "More", ContentDescription: "Delete account",
		ClassName: "android.widget.Button", Clickable: true, Enabled: true, VisibleToUser: true,
		Bounds: Bounds{Left: 10, Top: 10, Right: 180, Bottom: 60},
	}
	risk, effect := classifySurfaceAction(snapshot, described, ActionClick)
	if risk != "high" || effect != "high_risk" {
		t.Fatalf("Text/ContentDescription conflict classified as %q/%q", risk, effect)
	}
	label := surfaceActionLabel(snapshot, described, ActionClick)
	if !strings.Contains(label, "More") || !strings.Contains(label, "Delete account") {
		t.Fatalf("advertised label hid semantic evidence: %q", label)
	}
	bound := bindSurfaceAction(
		"surface-1", snapshot, described, ActionClick, label, risk, effect,
		surfaceContextDigest(snapshot),
	)
	if !bound.advertised.Disabled {
		t.Fatal("description-backed destructive action was not disabled")
	}

	parent := Node{
		ID: "0/destructive", ClassName: "android.view.View", Clickable: true, Enabled: true,
		VisibleToUser: true, Bounds: Bounds{Left: 10, Top: 70, Right: 220, Bottom: 130},
	}
	child := Node{
		ID: "0/destructive/0", ParentID: parent.ID, ClassName: "android.widget.TextView",
		Text: "Delete", Enabled: true, VisibleToUser: true,
		Bounds: Bounds{Left: 30, Top: 80, Right: 190, Bottom: 120},
	}
	snapshot.Nodes = append(snapshot.Nodes, parent, child)
	risk, effect = classifySurfaceAction(snapshot, parent, ActionClick)
	if risk != "high" || effect != "high_risk" {
		t.Fatalf("bounded child destructive evidence classified as %q/%q", risk, effect)
	}
	label = surfaceActionLabel(snapshot, parent, ActionClick)
	if !strings.Contains(label, "Delete") {
		t.Fatalf("child-backed advertised label = %q", label)
	}
	bound = bindSurfaceAction(
		"surface-1", snapshot, parent, ActionClick, label, risk, effect,
		surfaceContextDigest(snapshot),
	)
	if !bound.advertised.Disabled {
		t.Fatal("child-backed destructive action was not disabled")
	}
}

func TestSurfaceActionIDIsStaleAcrossPagesThatReuseNodeID(t *testing.T) {
	pageA := testSurfaceSnapshot(10, 7, testConversationActivity)
	pageA.Nodes = append(pageA.Nodes,
		Node{
			ID: "0/button", Text: "Details", Clickable: true, Enabled: true, VisibleToUser: true,
			Bounds: Bounds{Left: 10, Top: 10, Right: 60, Bottom: 30},
		},
		Node{ID: "0/page", Text: "Page A", VisibleToUser: true, Bounds: Bounds{Left: 1, Top: 101, Right: 60, Bottom: 120}},
	)
	driver, transport := newSurfaceTestDriver(t, pageA, testConversationActivity)
	driver.surfaces["surface-1"] = testSurfaceState(pageA)

	first, err := driver.SnapshotSurface(context.Background(), "surface-1")
	if err != nil {
		t.Fatal(err)
	}
	oldAction := requireSurfaceAction(t, first, "Details", ActionClick)

	pageB := pageA
	pageB.Sequence = 11
	pageB.Nodes = append([]Node(nil), pageA.Nodes...)
	pageB.Nodes[len(pageB.Nodes)-1].Text = "Page B"
	transport.setSnapshot(pageB, testConversationActivity)
	second, err := driver.SnapshotSurface(context.Background(), "surface-1")
	if err != nil {
		t.Fatal(err)
	}
	newAction := requireSurfaceAction(t, second, "Details", ActionClick)
	if oldAction.ID == newAction.ID {
		t.Fatalf("same node ID was rebound across pages: %q", oldAction.ID)
	}
	if _, err := driver.ActSurface(context.Background(), "surface-1", core.SurfaceAction{ActionID: oldAction.ID}); !errors.Is(err, ErrStale) {
		t.Fatalf("old page action error = %v, want ErrStale", err)
	}
	if actions := transport.recordedActions(); len(actions) != 0 {
		t.Fatalf("stale page action reached companion: %#v", actions)
	}
}

func TestUncertainUnknownSurfaceActionPermanentlyTombstonesReplay(t *testing.T) {
	pageA := testSurfaceSnapshot(10, 7, testConversationActivity)
	pageA.Nodes = append(pageA.Nodes,
		Node{
			ID: "0/mystery", Text: "Mystery", Clickable: true, Enabled: true, VisibleToUser: true,
			Bounds: Bounds{Left: 10, Top: 10, Right: 60, Bottom: 30},
		},
		Node{ID: "0/page", Text: "Context A", VisibleToUser: true, Bounds: Bounds{Left: 1, Top: 101, Right: 60, Bottom: 120}},
	)
	driver, transport := newSurfaceTestDriver(t, pageA, testConversationActivity)
	driver.surfaces["surface-1"] = testSurfaceState(pageA)
	first, err := driver.SnapshotSurface(context.Background(), "surface-1")
	if err != nil {
		t.Fatal(err)
	}
	action := requireSurfaceAction(t, first, "Mystery", ActionClick)
	if action.Risk != "medium" || action.Effect != "unknown" {
		t.Fatalf("unknown action classification = %#v", action)
	}
	if _, err := driver.ActSurface(context.Background(), "surface-1", core.SurfaceAction{ActionID: action.ID}); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("unconfirmed unknown action error = %v, want ErrConfirmationRequired", err)
	}
	if actions := transport.recordedActions(); len(actions) != 0 {
		t.Fatalf("unconfirmed unknown action reached companion: %#v", actions)
	}
	transport.setActionStatus(http.StatusInternalServerError)
	_, err = driver.ActSurface(context.Background(), "surface-1", core.SurfaceAction{
		ActionID: action.ID, Confirmed: true,
	})
	if !errors.Is(err, ErrActionOutcomeUncertain) {
		t.Fatalf("uncertain unknown action error = %v, want ErrActionOutcomeUncertain", err)
	}

	pageB := pageA
	pageB.Sequence = 11
	pageB.Nodes = append([]Node(nil), pageA.Nodes...)
	pageB.Nodes[len(pageB.Nodes)-1].Text = "Context B"
	transport.setActionStatus(0)
	transport.setSnapshot(pageB, testConversationActivity)
	second, err := driver.SnapshotSurface(context.Background(), "surface-1")
	if err != nil {
		t.Fatal(err)
	}
	if hasSurfaceAction(second, "Mystery", ActionClick) {
		t.Fatalf("tombstoned unknown action reappeared in a fresh context: %#v", second.Actions)
	}
	if _, err := driver.ActSurface(context.Background(), "surface-1", core.SurfaceAction{
		ActionID: action.ID, Confirmed: true,
	}); !errors.Is(err, ErrStale) {
		t.Fatalf("uncertain action retry error = %v, want ErrStale", err)
	}
	if actions := transport.recordedActions(); len(actions) != 1 {
		t.Fatalf("uncertain action dispatched %d times: %#v", len(actions), actions)
	}
}

func TestSuccessfulNavigationCanAdvertiseFreshContextualAction(t *testing.T) {
	pageA := testSurfaceSnapshot(10, 7, testConversationActivity)
	pageA.Nodes = append(pageA.Nodes,
		Node{
			ID: "0/details", Text: "Details", Clickable: true, Enabled: true, VisibleToUser: true,
			Bounds: Bounds{Left: 10, Top: 10, Right: 60, Bottom: 30},
		},
		Node{ID: "0/page", Text: "Context A", VisibleToUser: true, Bounds: Bounds{Left: 1, Top: 101, Right: 60, Bottom: 120}},
	)
	pageB := pageA
	pageB.Sequence = 11
	pageB.Nodes = append([]Node(nil), pageA.Nodes...)
	pageB.Nodes[len(pageB.Nodes)-1].Text = "Context B"
	driver, transport := newSurfaceTestDriver(t, pageA, testConversationActivity)
	driver.surfaces["surface-1"] = testSurfaceState(pageA)
	first, err := driver.SnapshotSurface(context.Background(), "surface-1")
	if err != nil {
		t.Fatal(err)
	}
	oldAction := requireSurfaceAction(t, first, "Details", ActionClick)
	if oldAction.Risk != "low" || oldAction.Effect != "navigate" {
		t.Fatalf("navigation classification = %#v", oldAction)
	}
	transport.actionTransition = &pageB
	transport.actionActivity = testConversationActivity
	second, err := driver.ActSurface(context.Background(), "surface-1", core.SurfaceAction{ActionID: oldAction.ID})
	if err != nil {
		t.Fatal(err)
	}
	newAction := requireSurfaceAction(t, second, "Details", ActionClick)
	if newAction.ID == oldAction.ID {
		t.Fatalf("successful navigation reused contextual action ID %q", oldAction.ID)
	}
	if _, err := driver.ActSurface(context.Background(), "surface-1", core.SurfaceAction{ActionID: oldAction.ID}); !errors.Is(err, ErrStale) {
		t.Fatalf("old navigation action error = %v, want ErrStale", err)
	}
}

func TestUncertainObserveActionCanRecoverOnlyFromFreshSnapshot(t *testing.T) {
	pageA := testSurfaceSnapshot(10, 7, testConversationActivity)
	driver, transport := newSurfaceTestDriver(t, pageA, testConversationActivity)
	driver.surfaces["surface-1"] = testSurfaceState(pageA)
	first, err := driver.SnapshotSurface(context.Background(), "surface-1")
	if err != nil {
		t.Fatal(err)
	}
	oldAction := requireSurfaceAction(t, first, "Scroll forward: Project updates", ActionScrollForward)
	transport.setActionStatus(http.StatusInternalServerError)
	if _, err := driver.ActSurface(context.Background(), "surface-1", core.SurfaceAction{ActionID: oldAction.ID}); !errors.Is(err, ErrActionOutcomeUncertain) {
		t.Fatalf("uncertain observe error = %v, want ErrActionOutcomeUncertain", err)
	}
	pageB := pageA
	pageB.Sequence = 11
	transport.setActionStatus(0)
	transport.setSnapshot(pageB, testConversationActivity)
	second, err := driver.SnapshotSurface(context.Background(), "surface-1")
	if err != nil {
		t.Fatal(err)
	}
	newAction := requireSurfaceAction(t, second, "Scroll forward: Project updates", ActionScrollForward)
	if newAction.ID == oldAction.ID {
		t.Fatalf("fresh observe snapshot reused consumed action ID %q", oldAction.ID)
	}
}

func TestCloseSurfaceRejectsStaleOrSensitiveWindowWithoutBack(t *testing.T) {
	bound := testSurfaceSnapshot(10, 7, testConversationActivity)
	tests := []struct {
		name     string
		current  UISnapshot
		activity string
		want     error
	}{
		{name: "sequence changed", current: testSurfaceSnapshot(11, 7, testConversationActivity), activity: testConversationActivity, want: ErrStale},
		{name: "window ID changed", current: testSurfaceSnapshot(10, 8, testConversationActivity), activity: testConversationActivity, want: ErrStale},
		{name: "activity changed", current: testSurfaceSnapshot(10, 7, "com.tencent.wework.contacts.controller.ContactActivity"), activity: "com.tencent.wework.contacts.controller.ContactActivity", want: ErrStale},
		{name: "foreign package", current: UISnapshot{Sequence: 10, PackageName: "com.android.systemui", WindowID: 7}, activity: testConversationActivity, want: ErrTargetAmbiguous},
		{
			name: "login screen",
			current: UISnapshot{
				Sequence: 10, PackageName: DefaultWeComPackage, WindowID: 7,
				Nodes: []Node{{ID: "0/agreement", ClassName: androidImageViewClass, ViewID: weComLoginAgreementViewID}},
			},
			activity: weComLoginWxAuthActivity,
			want:     ErrAuthRequired,
		},
		{
			name:     "risk verification",
			current:  structuredRiskOverlaySnapshot(10, 7),
			activity: testConversationActivity,
			want:     ErrUserActionRequired,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			driver, transport := newSurfaceTestDriver(t, test.current, test.activity)
			driver.surfaces["surface-1"] = testSurfaceState(bound)

			err := driver.CloseSurface(context.Background(), "surface-1")
			if !errors.Is(err, test.want) {
				t.Fatalf("CloseSurface() error = %v, want %v", err, test.want)
			}
			if actions := transport.recordedActions(); len(actions) != 0 {
				t.Fatalf("unsafe close sent global_back: %#v", actions)
			}
			if _, exists := driver.surfaces["surface-1"]; !exists {
				t.Fatal("rejected close removed the surface")
			}
		})
	}
}

func TestCloseSurfaceSendsSequenceBoundBackForExactWindow(t *testing.T) {
	current := testSurfaceSnapshot(10, 7, testConversationActivity)
	driver, transport := newSurfaceTestDriver(t, current, testConversationActivity)
	driver.surfaces["surface-1"] = testSurfaceState(current)

	if err := driver.CloseSurface(context.Background(), "surface-1"); err != nil {
		t.Fatal(err)
	}
	actions := transport.recordedActions()
	if len(actions) != 1 || actions[0].Kind != ActionGlobalBack || actions[0].ExpectedSequence != current.Sequence ||
		actions[0].NodeID != "" || actions[0].Text != "" {
		t.Fatalf("close action = %#v", actions)
	}
	if _, exists := driver.surfaces["surface-1"]; exists {
		t.Fatal("successfully closed surface remained recorded")
	}
}

func testSurfaceSnapshot(sequence int64, windowID int, activity string) UISnapshot {
	return UISnapshot{
		Sequence: sequence, PackageName: DefaultWeComPackage, WindowID: windowID,
		WindowTitle: "Project", WindowClass: activity,
		Nodes: []Node{{
			ID: "0/list", Text: "Project updates", Scrollable: true,
			Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 1, Top: 1, Right: 100, Bottom: 100},
		}},
	}
}

func structuredRiskOverlaySnapshot(sequence int64, windowID int) UISnapshot {
	return UISnapshot{
		Sequence: sequence, PackageName: DefaultWeComPackage, WindowID: windowID,
		WindowClass: testConversationActivity,
		Nodes: []Node{
			{ID: "0", VisibleToUser: true, Bounds: Bounds{Left: 0, Top: 0, Right: 1080, Bottom: 1920}},
			{ID: "0/9", ParentID: "0", VisibleToUser: true, Bounds: Bounds{Left: 120, Top: 420, Right: 960, Bottom: 1120}},
			{ID: "0/9/0", ParentID: "0/9", Text: "设备验证", VisibleToUser: true, Bounds: Bounds{Left: 180, Top: 500, Right: 900, Bottom: 580}},
			{ID: "0/9/1", ParentID: "0/9", Text: "开始验证", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 300, Top: 900, Right: 780, Bottom: 1000}},
		},
	}
}

func testPrivacyAuthSnapshot(sequence int64, windowID int) UISnapshot {
	return UISnapshot{
		Sequence: sequence, PackageName: DefaultWeComPackage, WindowID: windowID,
		WindowClass: weComLoginWxAuthActivity,
		Nodes: []Node{
			{ID: "0/0", Text: "Privacy Policy", Enabled: true, VisibleToUser: true},
			{ID: "0/1", Text: "Welcome to WeCom!", Enabled: true, VisibleToUser: true},
			{ID: "0/2", Text: "Agree", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 10, Top: 10, Right: 100, Bottom: 50}},
			{ID: "0/3", Text: "Disagree", Clickable: true, Enabled: true, VisibleToUser: true, Bounds: Bounds{Left: 10, Top: 60, Right: 100, Bottom: 100}},
		},
	}
}

func requireSingleImageBoundAuthAction(t *testing.T, snapshot core.AuthSnapshot) core.AuthAction {
	t.Helper()
	if len(snapshot.Actions) != 1 || !snapshot.Actions[0].ImageBound || snapshot.Actions[0].ID == "" {
		t.Fatalf("authentication snapshot has no unique image-bound action: %#v", snapshot.Actions)
	}
	return snapshot.Actions[0]
}

func testSurfaceState(snapshot UISnapshot) surfaceState {
	return surfaceState{
		surface: core.Surface{ID: "surface-1"},
		actions: make(map[string]surfaceActionState), consumedActions: make(map[string]struct{}),
		replayTombstones: make(map[string]struct{}), sequence: snapshot.Sequence,
		identity: identityForSurface(snapshot),
	}
}

type surfaceTestTransport struct {
	mu                   sync.Mutex
	snapshot             UISnapshot
	activity             string
	activityErr          error
	screenshotTransition *UISnapshot
	screenshotActivity   string
	actionTransition     *UISnapshot
	actionActivity       string
	actionStatus         int
	screenshotPNG        []byte
	screenshots          int
	actions              []CompanionAction
}

func newSurfaceTestDriver(t *testing.T, snapshot UISnapshot, activity string) (*Driver, *surfaceTestTransport) {
	t.Helper()
	transport := &surfaceTestTransport{snapshot: snapshot, activity: activity}
	server := httptest.NewServer(http.HandlerFunc(transport.serveHTTP))
	t.Cleanup(server.Close)
	const token = "abcdefghijklmnopqrstuvwxyzABCDEFGH0123456789"
	client, err := newCompanionClientForURL(server.URL, token, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{
		config: DefaultConfig(), companion: client, running: true,
		android: AndroidContainer{
			DockerBinary: "docker", Container: "surface-test", Executor: transport,
			Verify: func(context.Context) error { return nil },
		},
	}
	return &Driver{
		runtime: runtime, surfaces: make(map[string]surfaceState), sendMemos: make(map[string]sendMemo),
	}, transport
}

func (transport *surfaceTestTransport) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	transport.mu.Lock()
	defer transport.mu.Unlock()
	switch request.URL.Path {
	case "/v1/snapshot":
		_ = json.NewEncoder(writer).Encode(transport.snapshot)
	case "/v1/actions":
		var action CompanionAction
		if err := json.NewDecoder(request.Body).Decode(&action); err != nil {
			http.Error(writer, "invalid action", http.StatusBadRequest)
			return
		}
		transport.actions = append(transport.actions, action)
		if transport.actionTransition != nil {
			transport.snapshot = *transport.actionTransition
			transport.activity = transport.actionActivity
		}
		if transport.actionStatus != 0 {
			writer.WriteHeader(transport.actionStatus)
			_, _ = writer.Write([]byte(`{"error":"outcome uncertain"}`))
			return
		}
		_ = json.NewEncoder(writer).Encode(ActionResult{
			Accepted: true, Sequence: transport.snapshot.Sequence, Detail: "accepted",
		})
	default:
		http.NotFound(writer, request)
	}
}

func (transport *surfaceTestTransport) setSnapshot(snapshot UISnapshot, activity string) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	transport.snapshot = snapshot
	transport.activity = activity
}

func (transport *surfaceTestTransport) setActionStatus(status int) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	transport.actionStatus = status
}

func (transport *surfaceTestTransport) setScreenshotPNG(png []byte) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	transport.screenshotPNG = append([]byte(nil), png...)
}

func (transport *surfaceTestTransport) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, resumedActivityProbe):
		if transport.activityErr != nil {
			return nil, transport.activityErr
		}
		return resumedActivityOutput(transport.activity), nil
	case strings.Contains(joined, "/system/bin/screencap"):
		transport.screenshots++
		if transport.screenshotTransition != nil {
			transport.snapshot = *transport.screenshotTransition
			transport.activity = transport.screenshotActivity
		}
		if len(transport.screenshotPNG) != 0 {
			return append([]byte(nil), transport.screenshotPNG...), nil
		}
		return []byte("\x89PNG\r\n\x1a\nsynthetic"), nil
	default:
		return nil, fmt.Errorf("unexpected Android command: %s", joined)
	}
}

func (transport *surfaceTestTransport) RunInput(ctx context.Context, _ []byte, _ int64, name string, args ...string) ([]byte, error) {
	return transport.Run(ctx, name, args...)
}

func (transport *surfaceTestTransport) screenshotCount() int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.screenshots
}

func (transport *surfaceTestTransport) recordedActions() []CompanionAction {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return append([]CompanionAction(nil), transport.actions...)
}

func requireSurfaceAction(t *testing.T, surface core.Surface, label, kind string) core.Action {
	t.Helper()
	for _, action := range surface.Actions {
		if action.Label == label && action.Kind == kind {
			return action
		}
	}
	t.Fatalf("surface has no %q/%q action: %#v", label, kind, surface.Actions)
	return core.Action{}
}

func hasSurfaceAction(surface core.Surface, label, kind string) bool {
	for _, action := range surface.Actions {
		if action.Label == label && action.Kind == kind {
			return true
		}
	}
	return false
}
