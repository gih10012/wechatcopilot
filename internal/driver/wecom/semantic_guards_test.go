package wecom

import "testing"

func TestOrdinaryConversationMentionsDoNotBecomeAuthenticationOrRiskUI(t *testing.T) {
	snapshot := testConversationFrame(21, "Project", "older message", "", false, false)
	snapshot.Nodes = append(snapshot.Nodes, Node{
		ID: "0/1/9", ParentID: "0/1", ClassName: "android.widget.TextView",
		Text:      "讨论：设备验证、安全验证、验证码、扫码登录、隐私政策、同意协议、支付、转账和银行卡",
		Clickable: true, Enabled: true, VisibleToUser: true,
		Bounds: Bounds{Left: 80, Top: 700, Right: 850, Bottom: 820},
	})

	if authenticationSurface(snapshot) {
		t.Fatal("ordinary conversation copy was classified as authentication UI")
	}
	if snapshotRequiresUserAction(snapshot) {
		t.Fatal("ordinary conversation copy was classified as a security challenge")
	}
	if surfaceHighRisk(snapshot) {
		t.Fatal("ordinary conversation copy was classified as a high-risk surface")
	}
	if _, err := validateChatFrame(snapshot, "Project", nil, false); err != nil {
		t.Fatalf("ordinary conversation became unreadable: %v", err)
	}
}

func TestOrdinaryClickableConversationContainerDoesNotBorrowGlobalRisk(t *testing.T) {
	snapshot := testConversationFrame(25, "Project", "older message", "", false, false)
	container := Node{
		ID: "0/1/9", ParentID: "0/1", ClassName: "android.view.View",
		Clickable: true, Enabled: true, VisibleToUser: true,
		Bounds: Bounds{Left: 60, Top: 680, Right: 1020, Bottom: 840},
	}
	copy := Node{
		ID: "0/1/9/0", ParentID: container.ID, ClassName: "android.widget.TextView",
		Text:    "讨论支付、转账与删除账号这些词在文档里的含义，并不要求执行操作",
		Enabled: true, VisibleToUser: true,
		Bounds: Bounds{Left: 80, Top: 700, Right: 990, Bottom: 820},
	}
	snapshot.Nodes = append(snapshot.Nodes, container, copy)

	if structuredHighRiskSurface(snapshot) {
		t.Fatal("ordinary nested chat copy became a high-risk control")
	}
	risk, effect := classifySurfaceAction(snapshot, container, ActionClick)
	if risk == "high" || effect == "high_risk" {
		t.Fatalf("ordinary nested chat action classified as %q/%q", risk, effect)
	}
}

func TestSecurityChallengeUsesContentDescriptionAndChildActionLabel(t *testing.T) {
	snapshot := testConversationFrame(26, "Project", "older message", "", false, false)
	region := Node{
		ID: "0/challenge", ParentID: "0", ClassName: "android.view.View",
		Enabled: true, VisibleToUser: true,
		Bounds: Bounds{Left: 100, Top: 200, Right: 980, Bottom: 700},
	}
	marker := Node{
		ID: "0/challenge/marker", ParentID: region.ID, Text: "Notice",
		ContentDescription: "Device verification", Enabled: true, VisibleToUser: true,
		Bounds: Bounds{Left: 180, Top: 260, Right: 900, Bottom: 340},
	}
	action := Node{
		ID: "0/challenge/action", ParentID: region.ID, ClassName: "android.view.View",
		Clickable: true, Enabled: true, VisibleToUser: true,
		Bounds: Bounds{Left: 300, Top: 500, Right: 760, Bottom: 620},
	}
	actionLabel := Node{
		ID: "0/challenge/action/0", ParentID: action.ID, Text: "Verify",
		Enabled: true, VisibleToUser: true,
		Bounds: Bounds{Left: 340, Top: 530, Right: 720, Bottom: 590},
	}
	snapshot.Nodes = append(snapshot.Nodes, region, marker, action, actionLabel)
	if !structuredSecurityChallenge(snapshot) {
		t.Fatal("description-backed challenge with child-labeled action was not detected")
	}
}

func TestStructuredConversationChallengeAndPaymentControlStillFailClosed(t *testing.T) {
	challenge := structuredRiskOverlaySnapshot(22, 7)
	if !snapshotRequiresUserAction(challenge) {
		t.Fatal("structured challenge overlay was not detected")
	}

	payment := testConversationFrame(23, "Project", "older message", "", false, false)
	payment.Nodes = append(payment.Nodes, Node{
		ID: "0/1/8", ParentID: "0/1", Text: "确认支付", Clickable: true, Enabled: true,
		VisibleToUser: true, Bounds: Bounds{Left: 700, Top: 900, Right: 1000, Bottom: 980},
	})
	if !surfaceHighRisk(payment) {
		t.Fatal("actionable payment control was not detected")
	}
}

func TestAuthenticationFallbackRequiresStructuredEvidence(t *testing.T) {
	plain := UISnapshot{
		PackageName: DefaultWeComPackage,
		Nodes: []Node{{
			ID: "0/message", Text: "Scan with WeChat and enter verification code", VisibleToUser: true,
			Bounds: Bounds{Left: 20, Top: 20, Right: 500, Bottom: 80},
		}},
	}
	if authenticationSurface(plain) {
		t.Fatal("unstructured authentication words were trusted as an authentication screen")
	}

	qr := plain
	qr.Nodes = append(qr.Nodes, Node{
		ID: "0/qr", ClassName: "android.widget.ImageView", VisibleToUser: true,
		Bounds: Bounds{Left: 100, Top: 100, Right: 400, Bottom: 400},
	})
	if !authenticationSurface(qr) {
		t.Fatal("structured QR fallback was not detected")
	}
}

func TestOutgoingBubbleEvidenceRequiresRightAlignment(t *testing.T) {
	base := testConversationFrame(24, "Project", "other", "", false, false)
	frame, err := validateChatFrame(base, "Project", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	left := base
	left.Nodes = append(append([]Node(nil), base.Nodes...),
		Node{ID: "0/1/7", ParentID: "0/1", VisibleToUser: true, Bounds: Bounds{Left: 40, Top: 800, Right: 430, Bottom: 900}},
		Node{ID: "0/1/7/0", ParentID: "0/1/7", Text: "target", VisibleToUser: true, Bounds: Bounds{Left: 70, Top: 820, Right: 400, Bottom: 880}},
	)
	if evidence := outgoingBubbleEvidence(left, "target", frame.binding); len(evidence) != 0 {
		t.Fatalf("left-aligned incoming text was accepted as outgoing evidence: %#v", evidence)
	}

	right := left
	right.Nodes = append(append([]Node(nil), left.Nodes...),
		Node{ID: "0/1/8", ParentID: "0/1", VisibleToUser: true, Bounds: Bounds{Left: 650, Top: 1000, Right: 1040, Bottom: 1100}},
		Node{ID: "0/1/8/0", ParentID: "0/1/8", Text: "target", VisibleToUser: true, Bounds: Bounds{Left: 690, Top: 1020, Right: 1010, Bottom: 1080}},
	)
	if evidence := outgoingBubbleEvidence(right, "target", frame.binding); len(evidence) != 1 {
		t.Fatalf("right-aligned outgoing evidence = %#v", evidence)
	}
}
