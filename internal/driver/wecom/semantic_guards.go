package wecom

import "strings"

var securityChallengeMarkers = []string{
	"账号存在风险", "账户存在风险", "设备验证", "安全验证", "异常登录", "确认本人操作",
	"account risk", "device verification", "security verification", "unusual login", "confirm on your phone",
	"人脸验证", "人脸识别", "滑块验证", "完成验证", "face verification", "captcha",
}

var securityChallengeActionLabels = []string{
	"确认本人操作", "开始验证", "去验证", "继续验证", "完成验证", "重新验证",
	"verify", "start verification", "continue", "confirm", "retry",
}

var securityMetadataMarkers = []string{
	"verify", "verification", "captcha", "slider", "security", "risk", "face_auth", "faceauth",
}

var highRiskActivityMarkers = []string{
	"pay", "payment", "wallet", "transfer", "cash", "bankcard", "bank_card", "checkout",
	"purchase", "orderpay", "authorize", "authorization", "permission",
}

func structuredSecurityChallenge(snapshot UISnapshot) bool {
	if snapshot.PackageName != DefaultWeComPackage {
		return false
	}
	activity := strings.ToLower(strings.TrimSpace(snapshot.WindowClass))
	if activity != "" && activity != strings.ToLower(weComConversationActivity) &&
		containsAny(activity, securityMetadataMarkers...) {
		return true
	}

	markers := matchingNodes(snapshot, func(node Node) bool {
		return node.VisibleToUser && containsAny(nodeSemanticEvidence(node), securityChallengeMarkers...)
	})
	if len(markers) == 0 {
		return false
	}
	for _, node := range snapshot.Nodes {
		metadata := node.ClassName + " " + node.ViewID
		if node.VisibleToUser && usableBounds(node.Bounds) && containsAny(metadata, securityMetadataMarkers...) {
			return true
		}
	}

	// Known authentication Activities own their visible challenge copy; it is
	// not a chat message and must always hand control back to the user.
	if isWeComActivity(snapshot, weComLoginWxAuthActivity) || isWeComActivity(snapshot, weComSMSVerifyActivity) {
		return true
	}

	byID, err := uniqueNodeIndex(snapshot.Nodes)
	if err != nil {
		return true
	}
	for _, marker := range markers {
		for _, candidate := range snapshot.Nodes {
			if !candidate.VisibleToUser || !candidate.Enabled || !usableBounds(candidate.Bounds) {
				continue
			}
			isChallengeAction := candidate.Clickable && actionMatchesAny(snapshot, candidate, securityChallengeActionLabels...)
			isChallengeInput := candidate.Editable || candidate.Checkable
			if (isChallengeAction || isChallengeInput) && nodesShareBoundedRegion(marker, candidate, byID) {
				return true
			}
		}
	}
	return false
}

func structuredHighRiskSurface(snapshot UISnapshot) bool {
	if snapshot.PackageName != DefaultWeComPackage {
		return false
	}
	activity := strings.ToLower(strings.TrimSpace(snapshot.WindowClass))
	if activity != "" && containsAny(activity, highRiskActivityMarkers...) {
		return true
	}
	if !isWeComActivity(snapshot, weComConversationActivity) && sensitiveSurfaceLabel(snapshot.WindowTitle) {
		return true
	}
	conversation := isWeComActivity(snapshot, weComConversationActivity)
	for _, node := range snapshot.Nodes {
		if !node.VisibleToUser || !node.Enabled || !usableBounds(node.Bounds) {
			continue
		}
		// Composer text and message bubbles are user/chat content. In an exact
		// conversation Activity only a distinct clickable/checkable control can
		// turn a financial or authorization label into high-risk UI evidence.
		actionable := node.Clickable || node.Checkable || (!conversation && node.Editable)
		sensitive := sensitiveSurfaceLabel(actionSemanticEvidence(snapshot, node))
		if conversation {
			sensitive = sensitiveConversationControl(snapshot, node)
		}
		if actionable && sensitive {
			return true
		}
	}
	return false
}

func sensitiveConversationControl(snapshot UISnapshot, target Node) bool {
	for _, node := range actionSemanticNodes(snapshot, target) {
		for _, value := range nodeUserFacingValues(node) {
			if matchesAny(value,
				"支付", "立即支付", "确认支付", "付款", "确认付款", "转账", "确认转账", "发红包",
				"授权", "允许授权", "购买", "立即购买", "下单", "提交订单", "充值", "提现",
				"pay", "pay now", "confirm payment", "transfer", "send red packet", "authorize",
				"allow", "purchase", "buy now", "place order", "top up", "withdraw",
			) {
				return true
			}
		}
		if containsAny(node.ViewID,
			"pay", "payment", "wallet", "transfer", "redpacket", "red_packet", "authorize",
			"purchase", "checkout", "recharge", "withdraw",
		) {
			return true
		}
	}
	return false
}

func structuredSMSFallback(snapshot UISnapshot) bool {
	if snapshot.PackageName != DefaultWeComPackage || isWeComActivity(snapshot, weComConversationActivity) {
		return false
	}
	inputs := matchingNodes(snapshot, func(node Node) bool {
		return node.VisibleToUser && node.Enabled && node.Editable && usableBounds(node.Bounds)
	})
	if len(inputs) != 1 {
		return false
	}
	hasCodeMarker := false
	hasSubmit := false
	for _, node := range snapshot.Nodes {
		if !node.VisibleToUser {
			continue
		}
		hasCodeMarker = hasCodeMarker || containsAny(nodeSemanticEvidence(node), "verification code", "enter code", "sms code", "验证码", "短信验证码")
		hasSubmit = hasSubmit || (node.Enabled && node.Clickable && usableBounds(node.Bounds) &&
			actionMatchesAny(snapshot, node, "确定", "登录", "下一步", "验证", "submit", "continue", "verify"))
	}
	return hasCodeMarker && hasSubmit
}

func structuredQRFallback(snapshot UISnapshot) bool {
	if snapshot.PackageName != DefaultWeComPackage || isWeComActivity(snapshot, weComConversationActivity) {
		return false
	}
	hasMarker := false
	hasImage := false
	for _, node := range snapshot.Nodes {
		if !node.VisibleToUser || !usableBounds(node.Bounds) {
			continue
		}
		label := nodeSemanticEvidence(node)
		hasMarker = hasMarker || containsAny(label, "scan with wechat", "scan to log in", "登录二维码", "扫码登录")
		hasImage = hasImage || (containsAny(node.ClassName, "imageview", "image") &&
			boundsWidth(node.Bounds) >= 80 && boundsHeight(node.Bounds) >= 80)
	}
	return hasMarker && hasImage
}

func nodesShareBoundedRegion(left, right Node, byID map[string]Node) bool {
	ancestorID := commonAncestorID(left.ID, right.ID)
	if ancestorID == "" || ancestorID == "0" {
		return false
	}
	ancestor, ok := byID[ancestorID]
	return ok && ancestor.VisibleToUser && usableBounds(ancestor.Bounds) &&
		boundsContains(ancestor.Bounds, left.Bounds) && boundsContains(ancestor.Bounds, right.Bounds)
}

func commonAncestorID(left, right string) string {
	leftParts := strings.Split(left, "/")
	rightParts := strings.Split(right, "/")
	limit := len(leftParts)
	if len(rightParts) < limit {
		limit = len(rightParts)
	}
	shared := make([]string, 0, limit)
	for index := 0; index < limit && leftParts[index] == rightParts[index]; index++ {
		shared = append(shared, leftParts[index])
	}
	return strings.Join(shared, "/")
}
