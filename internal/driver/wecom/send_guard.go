package wecom

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const weComConversationActivity = "com.tencent.wework.msg.controller.ConversationActivity"

// chatBinding is the non-textual identity of the exact conversation frame in
// which a send was prepared. It deliberately includes the window, title
// header, bottom composer, and their common footer. A later snapshot may have
// different message text or composer contents, but it cannot silently migrate
// the pending send to another window or a look-alike control.
type chatBinding struct {
	identity          surfaceIdentity
	title             string
	viewport          Bounds
	headerID          string
	headerClass       string
	headerViewID      string
	headerBounds      Bounds
	headerScopeID     string
	headerScopeClass  string
	headerScopeViewID string
	headerScopeBounds Bounds
	composerID        string
	composerClass     string
	composerViewID    string
	composerBounds    Bounds
	footerID          string
	footerClass       string
	footerViewID      string
	footerBounds      Bounds
}

type chatFrame struct {
	snapshot UISnapshot
	binding  chatBinding
	composer Node
	send     Node
}

func validateChatFrame(snapshot UISnapshot, title string, expected *chatBinding, requireSend bool) (chatFrame, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return chatFrame{}, fmt.Errorf("%w: conversation title is empty", ErrTargetAmbiguous)
	}
	if snapshot.PackageName != DefaultWeComPackage {
		return chatFrame{}, fmt.Errorf("%w: exact official WeCom package is not foreground", ErrTargetAmbiguous)
	}
	if authenticationSurface(snapshot) {
		return chatFrame{}, ErrAuthRequired
	}
	if snapshotRequiresUserAction(snapshot) || surfaceHighRisk(snapshot) {
		return chatFrame{}, ErrUserActionRequired
	}
	if !isWeComActivity(snapshot, weComConversationActivity) {
		return chatFrame{}, fmt.Errorf("%w: exact official conversation Activity is not foreground", ErrTargetAmbiguous)
	}
	if snapshot.Sequence <= 0 || snapshot.WindowID < 0 {
		return chatFrame{}, fmt.Errorf("%w: conversation window identity is incomplete", ErrStale)
	}

	byID, err := uniqueNodeIndex(snapshot.Nodes)
	if err != nil {
		return chatFrame{}, err
	}
	root, ok := byID["0"]
	if !ok || !root.VisibleToUser || !usableBounds(root.Bounds) {
		return chatFrame{}, fmt.Errorf("%w: conversation viewport root is unavailable", ErrStale)
	}
	viewport := root.Bounds
	if boundsWidth(viewport) < 100 || boundsHeight(viewport) < 100 {
		return chatFrame{}, fmt.Errorf("%w: conversation viewport is too small", ErrStale)
	}

	headers := matchingNodes(snapshot, func(node Node) bool {
		if node.ID == "" || !node.VisibleToUser || node.Editable || !usableBounds(node.Bounds) ||
			normalizedVisibleLabel(nodeLabel(node)) != normalizedVisibleLabel(title) ||
			!boundsInside(viewport, node.Bounds) {
			return false
		}
		_, headerErr := boundedHeaderAncestor(node, byID, viewport)
		return headerErr == nil
	})
	if len(headers) == 0 {
		return chatFrame{}, fmt.Errorf("%w: exact conversation title is absent from the header", ErrTargetAmbiguous)
	}
	if len(headers) != 1 {
		return chatFrame{}, fmt.Errorf("%w: conversation header title is ambiguous", ErrTargetAmbiguous)
	}
	header := headers[0]
	headerScope, err := boundedHeaderAncestor(header, byID, viewport)
	if err != nil {
		return chatFrame{}, err
	}

	composers := matchingNodes(snapshot, func(node Node) bool {
		return node.ID != "" && node.VisibleToUser && node.Enabled && node.Editable &&
			usableBounds(node.Bounds) && boundsInside(viewport, node.Bounds) &&
			boundsCenterY(node.Bounds) >= viewport.Top+(boundsHeight(viewport)*3)/5
	})
	if len(composers) == 0 {
		return chatFrame{}, fmt.Errorf("%w: bottom message composer is unavailable", ErrNotFound)
	}
	if len(composers) != 1 {
		return chatFrame{}, fmt.Errorf("%w: bottom message composer is ambiguous", ErrTargetAmbiguous)
	}
	composer := composers[0]
	footer, err := boundedFooterAncestor(composer, byID, viewport)
	if err != nil {
		return chatFrame{}, err
	}
	if isDescendantOrSelf(header.ID, footer.ID) || header.Bounds.Bottom >= footer.Bounds.Top {
		return chatFrame{}, fmt.Errorf("%w: conversation header and footer overlap", ErrTargetAmbiguous)
	}

	binding := chatBinding{
		identity:          identityForSurface(snapshot),
		title:             normalizedVisibleLabel(title),
		viewport:          viewport,
		headerID:          header.ID,
		headerClass:       header.ClassName,
		headerViewID:      header.ViewID,
		headerBounds:      header.Bounds,
		headerScopeID:     headerScope.ID,
		headerScopeClass:  headerScope.ClassName,
		headerScopeViewID: headerScope.ViewID,
		headerScopeBounds: headerScope.Bounds,
		composerID:        composer.ID,
		composerClass:     composer.ClassName,
		composerViewID:    composer.ViewID,
		composerBounds:    composer.Bounds,
		footerID:          footer.ID,
		footerClass:       footer.ClassName,
		footerViewID:      footer.ViewID,
		footerBounds:      footer.Bounds,
	}
	if expected != nil && binding != *expected {
		return chatFrame{}, fmt.Errorf("%w: verified conversation frame changed", ErrStale)
	}

	send, sendErr := uniqueNearbySend(snapshot, binding)
	if requireSend && sendErr != nil {
		return chatFrame{}, sendErr
	}
	if !requireSend && sendErr != nil && !errors.Is(sendErr, ErrNotFound) {
		return chatFrame{}, sendErr
	}
	return chatFrame{snapshot: snapshot, binding: binding, composer: composer, send: send}, nil
}

func boundedHeaderAncestor(header Node, byID map[string]Node, viewport Bounds) (Node, error) {
	var candidates []Node
	parentID := header.ParentID
	for depth := 0; parentID != "" && depth <= len(byID); depth++ {
		parent, ok := byID[parentID]
		if !ok || parent.ID == header.ID {
			break
		}
		if parent.ID != "0" && parent.VisibleToUser && usableBounds(parent.Bounds) &&
			boundsInside(viewport, parent.Bounds) && boundsContains(parent.Bounds, header.Bounds) &&
			parent.Bounds.Top <= viewport.Top+boundsHeight(viewport)/10 &&
			parent.Bounds.Bottom <= viewport.Top+(boundsHeight(viewport)*2)/5 &&
			boundsHeight(parent.Bounds) <= (boundsHeight(viewport)*2)/5 {
			candidates = append(candidates, parent)
		}
		parentID = parent.ParentID
	}
	if len(candidates) == 0 {
		return Node{}, fmt.Errorf("%w: title is not contained by a bounded top header", ErrTargetAmbiguous)
	}
	return candidates[len(candidates)-1], nil
}

func uniqueNodeIndex(nodes []Node) (map[string]Node, error) {
	result := make(map[string]Node, len(nodes))
	for _, node := range nodes {
		if node.ID == "" {
			continue
		}
		if _, exists := result[node.ID]; exists {
			return nil, fmt.Errorf("%w: semantic tree contains duplicate node identities", ErrTargetAmbiguous)
		}
		result[node.ID] = node
	}
	return result, nil
}

func boundedFooterAncestor(composer Node, byID map[string]Node, viewport Bounds) (Node, error) {
	var candidates []Node
	parentID := composer.ParentID
	for depth := 0; parentID != "" && depth <= len(byID); depth++ {
		parent, ok := byID[parentID]
		if !ok || parent.ID == composer.ID {
			break
		}
		if parent.ID != "0" && parent.VisibleToUser && usableBounds(parent.Bounds) &&
			boundsInside(viewport, parent.Bounds) && boundsContains(parent.Bounds, composer.Bounds) &&
			parent.Bounds.Top >= viewport.Top+boundsHeight(viewport)/2 &&
			boundsHeight(parent.Bounds) <= boundsHeight(viewport)/2 {
			candidates = append(candidates, parent)
		}
		parentID = parent.ParentID
	}
	if len(candidates) == 0 {
		return Node{}, fmt.Errorf("%w: composer is not contained by a bounded bottom footer", ErrStale)
	}
	// The highest qualifying ancestor is the footer scope. Nested edit-text
	// wrappers are intentionally skipped so the adjacent send control must be
	// in the same bounded semantic region.
	footer := candidates[len(candidates)-1]
	return footer, nil
}

func uniqueNearbySend(snapshot UISnapshot, binding chatBinding) (Node, error) {
	var candidates []Node
	for _, node := range snapshot.Nodes {
		if node.ID == "" || !node.VisibleToUser || !node.Enabled || !node.Clickable ||
			!usableBounds(node.Bounds) || !matchesAny(nodeLabel(node), "发送", "send") ||
			!isDescendant(node.ID, binding.footerID) || !boundsInside(binding.footerBounds, node.Bounds) {
			continue
		}
		verticalOverlap := minInt(node.Bounds.Bottom, binding.composerBounds.Bottom) -
			maxInt(node.Bounds.Top, binding.composerBounds.Top)
		minHeight := minInt(boundsHeight(node.Bounds), boundsHeight(binding.composerBounds))
		horizontalGap := node.Bounds.Left - binding.composerBounds.Right
		if verticalOverlap*2 < minHeight || horizontalGap < -boundsWidth(binding.composerBounds)/5 ||
			horizontalGap > boundsWidth(binding.viewport)/4 ||
			boundsCenterX(node.Bounds) <= boundsCenterX(binding.composerBounds) {
			continue
		}
		candidates = append(candidates, node)
	}
	if len(candidates) == 0 {
		return Node{}, fmt.Errorf("%w: enabled send control is not adjacent to the bound composer", ErrNotFound)
	}
	if len(candidates) != 1 {
		return Node{}, fmt.Errorf("%w: adjacent send control is ambiguous", ErrTargetAmbiguous)
	}
	return candidates[0], nil
}

func outgoingBubbleEvidence(snapshot UISnapshot, text string, binding chatBinding) []string {
	byID, err := uniqueNodeIndex(snapshot.Nodes)
	if err != nil {
		return nil
	}
	matching := make(map[string]Node)
	for _, node := range snapshot.Nodes {
		if node.ID == "" || node.Editable || !node.VisibleToUser || !usableBounds(node.Bounds) ||
			node.Text != text || node.Bounds.Bottom > binding.footerBounds.Top ||
			isDescendantOrSelf(node.ID, binding.footerID) || isDescendantOrSelf(node.ID, binding.headerID) {
			continue
		}
		matching[node.ID] = node
	}

	// Accessibility trees sometimes expose the same text on both a bubble and
	// its TextView child. Keep the deepest exact-text node so one visual bubble
	// contributes one piece of evidence.
	for id := range matching {
		for otherID := range matching {
			if id != otherID && isDescendant(otherID, id) {
				delete(matching, id)
				break
			}
		}
	}

	result := make([]string, 0, len(matching))
	for _, node := range matching {
		visual, structurallyBounded := directionalBubbleContainer(node, byID, binding)
		if !structurallyBounded || !usableBounds(visual) || !rightAlignedWithin(visual, binding.viewport) {
			continue
		}
		result = append(result, strings.Join([]string{
			node.ID,
			fmt.Sprint(node.Bounds.Left, ",", node.Bounds.Top, ",", node.Bounds.Right, ",", node.Bounds.Bottom),
			fmt.Sprint(visual.Left, ",", visual.Top, ",", visual.Right, ",", visual.Bottom),
		}, "\x00"))
	}
	sort.Strings(result)
	return result
}

func directionalBubbleContainer(node Node, byID map[string]Node, binding chatBinding) (Bounds, bool) {
	best := node.Bounds
	found := false
	parentID := node.ParentID
	for depth := 0; parentID != "" && depth <= len(byID); depth++ {
		parent, ok := byID[parentID]
		if !ok || parent.ID == node.ID || parent.ID == binding.footerID || parent.ID == "0" {
			break
		}
		if parent.VisibleToUser && usableBounds(parent.Bounds) && boundsContains(parent.Bounds, node.Bounds) &&
			parent.Bounds.Bottom <= binding.footerBounds.Top &&
			boundsWidth(parent.Bounds)*10 <= boundsWidth(binding.viewport)*9 {
			best = parent.Bounds
			found = true
		}
		parentID = parent.ParentID
	}
	return best, found
}

func rightAlignedWithin(candidate, viewport Bounds) bool {
	if !boundsInside(viewport, candidate) {
		return false
	}
	leftGap := candidate.Left - viewport.Left
	rightGap := viewport.Right - candidate.Right
	return boundsCenterX(candidate) > boundsCenterX(viewport) &&
		leftGap > rightGap+boundsWidth(viewport)/10
}

func boundsInside(outer, inner Bounds) bool {
	return inner.Left >= outer.Left && inner.Top >= outer.Top &&
		inner.Right <= outer.Right && inner.Bottom <= outer.Bottom
}

func boundsContains(outer, inner Bounds) bool { return boundsInside(outer, inner) }

func boundsWidth(bounds Bounds) int   { return bounds.Right - bounds.Left }
func boundsHeight(bounds Bounds) int  { return bounds.Bottom - bounds.Top }
func boundsCenterX(bounds Bounds) int { return bounds.Left + boundsWidth(bounds)/2 }
func boundsCenterY(bounds Bounds) int { return bounds.Top + boundsHeight(bounds)/2 }

func isDescendant(id, ancestor string) bool {
	return ancestor != "" && id != ancestor && strings.HasPrefix(id, ancestor+"/")
}

func isDescendantOrSelf(id, ancestor string) bool {
	return id == ancestor || isDescendant(id, ancestor)
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
