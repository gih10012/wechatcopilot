package dev.wechatcopilot.companion

import android.accessibilityservice.AccessibilityService
import android.graphics.Rect
import android.os.Bundle
import android.os.Looper
import android.util.Log
import android.view.accessibility.AccessibilityEvent
import android.view.accessibility.AccessibilityNodeInfo
import java.time.Instant

internal fun canRequestCheck(
    visibleToUser: Boolean,
    enabled: Boolean,
    clickable: Boolean,
    checkable: Boolean,
    checked: Boolean,
): Boolean = visibleToUser && enabled && clickable && checkable && !checked

class WeComAccessibilityService : AccessibilityService() {
    private val mainThreadDispatcher by lazy {
        MainThreadDispatcher(AndroidMainThreadScheduler(), MAIN_THREAD_TIMEOUT_MILLIS)
    }
    private val snapshotGuard = SemanticSnapshotGuard()

    override fun onServiceConnected() {
        super.onServiceConnected()
        CompanionRuntime.attach(this)
    }

    override fun onAccessibilityEvent(event: AccessibilityEvent?) {
        if (event?.packageName?.toString() == CompanionRuntime.WECOM_PACKAGE) {
            if (event.eventType == AccessibilityEvent.TYPE_WINDOW_STATE_CHANGED) {
                CompanionRuntime.observeWindow(event.className?.toString().orEmpty(), event.windowId)
            } else {
                CompanionRuntime.markUiChanged()
            }
        }
    }

    override fun onInterrupt() = Unit

    override fun onDestroy() {
        CompanionRuntime.detach(this)
        super.onDestroy()
    }

    internal fun buildSnapshot(): UiSnapshotModel = when (
        val outcome = mainThreadDispatcher.dispatch { buildSnapshotOnMainThread() }
    ) {
        is DispatchOutcome.Completed -> outcome.value
        is DispatchOutcome.Failed -> {
            Log.w(TAG, "Semantic snapshot capture failed", outcome.cause)
            val requestError = outcome.cause as? RequestException
            throw requestError ?: RequestException(500, "semantic snapshot capture failed")
        }
        DispatchOutcome.CancelledBeforeStart,
        DispatchOutcome.SchedulerRejected,
        -> throw RequestException(503, "accessibility main thread is unavailable")
        DispatchOutcome.RunningAtDeadline ->
            throw RequestException(503, "semantic snapshot capture timed out")
    }

    internal fun performConstrainedAction(action: CompanionAction): ActionResult = when (
        val outcome = mainThreadDispatcher.dispatch { performConstrainedActionOnMainThread(action) }
    ) {
        is DispatchOutcome.Completed -> outcome.value
        is DispatchOutcome.Failed -> {
            Log.w(TAG, "Semantic action outcome is uncertain", outcome.cause)
            throw RequestException(503, "semantic action outcome is uncertain")
        }
        DispatchOutcome.CancelledBeforeStart,
        DispatchOutcome.SchedulerRejected,
        -> ActionResult(
            false,
            CompanionRuntime.currentSequence(),
            "action was not started because the accessibility main thread is unavailable",
        )
        DispatchOutcome.RunningAtDeadline ->
            throw RequestException(503, "semantic action outcome is uncertain")
    }

    private fun buildSnapshotOnMainThread(): UiSnapshotModel {
        requireMainThread()
        if (!CompanionRuntime.isAttached(this)) {
            throw RequestException(503, "accessibility service is not connected")
        }
        repeat(MAX_CAPTURE_ATTEMPTS) {
            val sequenceBefore = CompanionRuntime.currentSequence()
            val capture = captureCurrentTree()
            val sequenceAfter = CompanionRuntime.currentSequence()
            if (sequenceBefore != sequenceAfter) return@repeat
            if (!capture.fingerprint.complete) {
                throw RequestException(503, "semantic snapshot exceeds the safe traversal limit")
            }

            var snapshotSequence = sequenceAfter
            if (snapshotGuard.needsSequenceAdvance(snapshotSequence, capture.fingerprint)) {
                snapshotSequence = CompanionRuntime.advanceSequenceIfCurrent(snapshotSequence)
                    ?: return@repeat
            }
            snapshotGuard.record(snapshotSequence, capture.fingerprint)
            return capture.toSnapshot(snapshotSequence)
        }
        throw RequestException(503, "semantic snapshot changed during capture")
    }

    private fun performConstrainedActionOnMainThread(action: CompanionAction): ActionResult {
        requireMainThread()
        if (!CompanionRuntime.isAttached(this)) return rejected("accessibility service is not connected")

        var mutationStarted = false
        try {
            if (action.kind == "global_back") {
                mutationStarted = true
                val accepted = performGlobalAction(GLOBAL_ACTION_BACK)
                return actionResult(accepted, if (accepted) "back requested" else "back rejected")
            }

            val sequenceBefore = CompanionRuntime.currentSequence()
            if (action.expectedSequence != sequenceBefore) return rejected("semantic snapshot is stale")

            val root = rootInActiveWindow
                ?: return rejected("semantic tree is stale or missing")
            val capture = captureTree(root, action.nodeId)
            if (!capture.fingerprint.complete) {
                return rejected("semantic tree exceeds the safe traversal limit")
            }
            if (!snapshotGuard.matches(action.expectedSequence, sequenceBefore, capture.fingerprint)) {
                return rejected("semantic tree is stale or does not match the captured snapshot")
            }
            if (capture.fingerprint.packageName != CompanionRuntime.WECOM_PACKAGE) {
                return rejected("semantic tree is outside the official WeCom package")
            }
            // Refresh the same root and rebuild the complete context immediately before the
            // action. This catches remote-window changes whose accessibility event is still
            // queued behind this main-thread operation.
            if (!root.refresh()) return rejected("semantic root changed before action")
            val verifiedCapture = captureTree(root, action.nodeId)
            if (!verifiedCapture.fingerprint.complete) {
                return rejected("semantic tree became incomplete before action")
            }
            if (verifiedCapture.fingerprint != capture.fingerprint ||
                !snapshotGuard.matches(action.expectedSequence, sequenceBefore, verifiedCapture.fingerprint)
            ) {
                return rejected("semantic tree changed before action")
            }
            val target = verifiedCapture.targetNode
                ?: return rejected("semantic node is stale or missing")
            val targetModel = verifiedCapture.targetModel
                ?: return rejected("semantic node fingerprint is missing")
            if (target.packageName?.toString() != CompanionRuntime.WECOM_PACKAGE) {
                return rejected("semantic node is outside the official WeCom package")
            }
            if (!isActionAllowed(action, target)) {
                return rejected("action rejected by semantic node")
            }

            // Refresh and compare the exact node object that will receive the action. We never
            // resolve the path again, so a same-path replacement cannot inherit this request.
            if (!target.refresh() || nodeModel(target, targetModel.id, targetModel.parentId) != targetModel) {
                return rejected("semantic node changed before action")
            }
            val sequenceBeforeMutation = CompanionRuntime.currentSequence()
            if (sequenceBeforeMutation != action.expectedSequence) {
                return rejected("semantic snapshot changed before action")
            }
            if (!snapshotGuard.consume(
                    action.expectedSequence,
                    sequenceBeforeMutation,
                    verifiedCapture.fingerprint,
                )
            ) {
                return rejected("semantic snapshot is no longer actionable")
            }

            // Consuming the cached fingerprint happens before invoking Android. Even if the
            // response is lost, this exact tree cannot accept another mutation request.
            mutationStarted = true
            val accepted = invokeAction(action, target)
            return actionResult(
                accepted,
                if (accepted) "action requested" else "action rejected by semantic node",
            )
        } catch (error: Exception) {
            if (mutationStarted) throw ActionOutcomeUncertainException(error)
            Log.w(TAG, "Semantic action inspection failed closed", error)
            return rejected("semantic action inspection failed")
        }
    }

    private fun captureCurrentTree(targetPath: String? = null): CapturedTree {
        val root = rootInActiveWindow
            ?: return CapturedTree(
                SemanticTreeFingerprint("", -1, "", "", true, emptyList()),
                null,
                null,
            )
        return captureTree(root, targetPath)
    }

    private fun captureTree(root: AccessibilityNodeInfo, targetPath: String?): CapturedTree {
        val packageName = root.packageName?.toString().orEmpty()
        val windowId = root.windowId
        val windowTitle = root.window?.title?.toString().orEmpty()
        val windowClass = CompanionRuntime.currentWindowClass(packageName, windowId)
        if (packageName != CompanionRuntime.WECOM_PACKAGE) {
            return CapturedTree(
                SemanticTreeFingerprint(
                    packageName,
                    windowId,
                    windowTitle,
                    windowClass,
                    true,
                    emptyList(),
                ),
                null,
                null,
            )
        }

        val traversal = TraversalState(targetPath)
        traverse(root, "0", null, traversal, 0)
        return CapturedTree(
            fingerprint = SemanticTreeFingerprint(
                packageName = packageName,
                windowId = windowId,
                windowTitle = windowTitle,
                windowClass = windowClass,
                complete = !traversal.truncated,
                nodes = traversal.nodes.toList(),
            ),
            targetNode = traversal.targetNode,
            targetModel = traversal.targetModel,
        )
    }

    private fun traverse(
        node: AccessibilityNodeInfo,
        path: String,
        parentPath: String?,
        state: TraversalState,
        depth: Int,
    ) {
        if (depth > MAX_DEPTH || state.nodes.size >= MAX_NODES) {
            state.truncated = true
            return
        }
        val model = nodeModel(node, path, parentPath)
        state.nodes.add(model)
        if (path == state.targetPath) {
            state.targetNode = node
            state.targetModel = model
        }
        if (depth == MAX_DEPTH && node.childCount > 0) {
            state.truncated = true
            return
        }
        for (index in 0 until node.childCount) {
            if (state.nodes.size >= MAX_NODES) {
                state.truncated = true
                break
            }
            val child = node.getChild(index)
            if (child == null) {
                state.truncated = true
                break
            }
            traverse(child, "$path/$index", path, state, depth + 1)
        }
    }

    private fun nodeModel(node: AccessibilityNodeInfo, path: String, parentPath: String?): UiNodeModel {
        val bounds = Rect()
        node.getBoundsInScreen(bounds)
        return UiNodeModel(
            id = path,
            parentId = parentPath,
            className = node.className?.toString().orEmpty(),
            viewId = node.viewIdResourceName.orEmpty(),
            text = node.text?.toString().orEmpty(),
            contentDescription = node.contentDescription?.toString().orEmpty(),
            bounds = BoundsModel(bounds.left, bounds.top, bounds.right, bounds.bottom),
            clickable = node.isClickable,
            checkable = node.isCheckable,
            checked = node.isChecked,
            editable = node.isEditable,
            scrollable = node.isScrollable,
            enabled = node.isEnabled,
            focused = node.isFocused,
            visibleToUser = node.isVisibleToUser,
        )
    }

    private fun isActionAllowed(action: CompanionAction, node: AccessibilityNodeInfo): Boolean =
        when (action.kind) {
            "click" -> node.isVisibleToUser && node.isEnabled && node.isClickable
            "check" -> canRequestCheck(
                visibleToUser = node.isVisibleToUser,
                enabled = node.isEnabled,
                clickable = node.isClickable,
                checkable = node.isCheckable,
                checked = node.isChecked,
            )
            "set_text" ->
                node.isVisibleToUser && node.isEnabled && node.isEditable && action.text.length <= 32 * 1024
            "scroll_forward", "scroll_backward" ->
                node.isVisibleToUser && node.isEnabled && node.isScrollable
            else -> false
        }

    private fun invokeAction(action: CompanionAction, node: AccessibilityNodeInfo): Boolean =
        when (action.kind) {
            "click", "check" -> node.performAction(AccessibilityNodeInfo.ACTION_CLICK)
            "set_text" -> {
                val arguments = Bundle().apply {
                    putCharSequence(AccessibilityNodeInfo.ACTION_ARGUMENT_SET_TEXT_CHARSEQUENCE, action.text)
                }
                node.performAction(AccessibilityNodeInfo.ACTION_SET_TEXT, arguments)
            }
            "scroll_forward" -> node.performAction(AccessibilityNodeInfo.ACTION_SCROLL_FORWARD)
            "scroll_backward" -> node.performAction(AccessibilityNodeInfo.ACTION_SCROLL_BACKWARD)
            else -> false
        }

    private fun actionResult(accepted: Boolean, detail: String): ActionResult =
        ActionResult(accepted, CompanionRuntime.currentSequence(), detail)

    private fun rejected(detail: String): ActionResult = actionResult(false, detail)

    private fun requireMainThread() {
        check(Looper.myLooper() == Looper.getMainLooper()) {
            "accessibility tree access must run on the main thread"
        }
    }

    private data class CapturedTree(
        val fingerprint: SemanticTreeFingerprint,
        val targetNode: AccessibilityNodeInfo?,
        val targetModel: UiNodeModel?,
    ) {
        fun toSnapshot(sequence: Long): UiSnapshotModel = UiSnapshotModel(
            sequence = sequence,
            packageName = fingerprint.packageName,
            windowTitle = fingerprint.windowTitle,
            windowClass = fingerprint.windowClass,
            capturedAt = Instant.now(),
            nodes = fingerprint.nodes,
        )
    }

    private class TraversalState(val targetPath: String?) {
        val nodes = ArrayList<UiNodeModel>()
        var targetNode: AccessibilityNodeInfo? = null
        var targetModel: UiNodeModel? = null
        var truncated = false
    }

    private class ActionOutcomeUncertainException(cause: Throwable) : RuntimeException(cause)

    private companion object {
        const val TAG = "WCCCompanion"
        const val MAIN_THREAD_TIMEOUT_MILLIS = 5_000L
        const val MAX_CAPTURE_ATTEMPTS = 3
        const val MAX_DEPTH = 64
        const val MAX_NODES = 5_000
    }
}
