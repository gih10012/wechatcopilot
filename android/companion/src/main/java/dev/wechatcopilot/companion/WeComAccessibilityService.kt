package dev.wechatcopilot.companion

import android.accessibilityservice.AccessibilityService
import android.graphics.Rect
import android.os.Bundle
import android.view.accessibility.AccessibilityEvent
import android.view.accessibility.AccessibilityNodeInfo
import java.time.Instant

class WeComAccessibilityService : AccessibilityService() {
    override fun onServiceConnected() {
        super.onServiceConnected()
        CompanionRuntime.attach(this)
    }

    override fun onAccessibilityEvent(event: AccessibilityEvent?) {
        if (event?.packageName?.toString() == CompanionRuntime.WECOM_PACKAGE) {
            CompanionRuntime.markUiChanged()
        }
    }

    override fun onInterrupt() = Unit

    override fun onDestroy() {
        CompanionRuntime.detach(this)
        super.onDestroy()
    }

    internal fun buildSnapshot(): UiSnapshotModel {
        val root = rootInActiveWindow
            ?: return UiSnapshotModel(CompanionRuntime.currentSequence(), "", "", Instant.now(), emptyList())
        val nodes = ArrayList<UiNodeModel>()
        traverse(root, "0", null, nodes, 0)
        return UiSnapshotModel(
            sequence = CompanionRuntime.currentSequence(),
            packageName = root.packageName?.toString().orEmpty(),
            windowTitle = root.window?.title?.toString().orEmpty(),
            capturedAt = Instant.now(),
            nodes = nodes,
        )
    }

    internal fun performConstrainedAction(action: CompanionAction): ActionResult {
        if (action.kind == "global_back") {
            val accepted = performGlobalAction(GLOBAL_ACTION_BACK)
            return ActionResult(accepted, CompanionRuntime.currentSequence(), if (accepted) "back requested" else "back rejected")
        }
		if (action.expectedSequence != CompanionRuntime.currentSequence()) {
			return ActionResult(false, CompanionRuntime.currentSequence(), "semantic snapshot is stale")
		}
        val node = findNode(action.nodeId)
            ?: return ActionResult(false, CompanionRuntime.currentSequence(), "semantic node is stale or missing")
        val accepted = when (action.kind) {
            "click" -> node.isEnabled && node.isClickable && node.performAction(AccessibilityNodeInfo.ACTION_CLICK)
            "set_text" -> setText(node, action.text)
            "scroll_forward" -> node.isEnabled && node.isScrollable &&
                node.performAction(AccessibilityNodeInfo.ACTION_SCROLL_FORWARD)
            "scroll_backward" -> node.isEnabled && node.isScrollable &&
                node.performAction(AccessibilityNodeInfo.ACTION_SCROLL_BACKWARD)
            else -> false
        }
        return ActionResult(
            accepted,
            CompanionRuntime.currentSequence(),
            if (accepted) "action requested" else "action rejected by semantic node",
        )
    }

    private fun setText(node: AccessibilityNodeInfo, text: String): Boolean {
        if (!node.isEnabled || !node.isEditable || text.length > 32 * 1024) return false
        val arguments = Bundle().apply {
            putCharSequence(AccessibilityNodeInfo.ACTION_ARGUMENT_SET_TEXT_CHARSEQUENCE, text)
        }
        return node.performAction(AccessibilityNodeInfo.ACTION_SET_TEXT, arguments)
    }

    private fun findNode(path: String): AccessibilityNodeInfo? {
        val segments = path.split('/')
        if (segments.isEmpty() || segments.first() != "0") return null
        var current = rootInActiveWindow ?: return null
        for (segment in segments.drop(1)) {
            val index = segment.toIntOrNull() ?: return null
            if (index < 0 || index >= current.childCount) return null
            current = current.getChild(index) ?: return null
        }
        return current.takeIf { it.packageName?.toString() == CompanionRuntime.WECOM_PACKAGE }
    }

    private fun traverse(
        node: AccessibilityNodeInfo,
        path: String,
        parentPath: String?,
        result: MutableList<UiNodeModel>,
        depth: Int,
    ) {
        if (depth > MAX_DEPTH || result.size >= MAX_NODES) return
        val bounds = Rect()
        node.getBoundsInScreen(bounds)
        result.add(
            UiNodeModel(
                id = path,
                parentId = parentPath,
                className = node.className?.toString().orEmpty(),
                viewId = node.viewIdResourceName.orEmpty(),
                text = node.text?.toString().orEmpty(),
                contentDescription = node.contentDescription?.toString().orEmpty(),
                bounds = BoundsModel(bounds.left, bounds.top, bounds.right, bounds.bottom),
                clickable = node.isClickable,
                editable = node.isEditable,
                scrollable = node.isScrollable,
                enabled = node.isEnabled,
                focused = node.isFocused,
            ),
        )
        for (index in 0 until node.childCount) {
            if (result.size >= MAX_NODES) break
            node.getChild(index)?.let { child ->
                traverse(child, "$path/$index", path, result, depth + 1)
            }
        }
    }

    private companion object {
        const val MAX_DEPTH = 64
        const val MAX_NODES = 5_000
    }
}
