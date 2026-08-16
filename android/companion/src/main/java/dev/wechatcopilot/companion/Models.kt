package dev.wechatcopilot.companion

import android.app.PendingIntent
import org.json.JSONArray
import org.json.JSONObject
import java.time.Instant

internal data class BoundsModel(
    val left: Int,
    val top: Int,
    val right: Int,
    val bottom: Int,
) {
    fun toJson(): JSONObject = JSONObject()
        .put("left", left)
        .put("top", top)
        .put("right", right)
        .put("bottom", bottom)
}

internal data class UiNodeModel(
    val id: String,
    val parentId: String?,
    val className: String,
    val viewId: String,
    val text: String,
    val contentDescription: String,
    val bounds: BoundsModel,
    val clickable: Boolean,
    val checkable: Boolean,
    val checked: Boolean,
    val editable: Boolean,
    val scrollable: Boolean,
    val enabled: Boolean,
    val focused: Boolean,
    val visibleToUser: Boolean,
) {
    fun toJson(): JSONObject = JSONObject()
        .put("id", id)
        .put("parent_id", parentId ?: "")
        .put("class_name", className)
        .put("view_id", viewId)
        .put("text", text)
        .put("content_description", contentDescription)
        .put("bounds", bounds.toJson())
        .put("clickable", clickable)
        .put("checkable", checkable)
        .put("checked", checked)
        .put("editable", editable)
        .put("scrollable", scrollable)
        .put("enabled", enabled)
        .put("focused", focused)
        .put("visible_to_user", visibleToUser)
}

internal data class UiSnapshotModel(
    val sequence: Long,
    val packageName: String,
    val windowTitle: String,
    val windowClass: String,
    val capturedAt: Instant,
    val nodes: List<UiNodeModel>,
) {
    fun toJson(): JSONObject {
        val encodedNodes = JSONArray()
        nodes.forEach { encodedNodes.put(it.toJson()) }
        return JSONObject()
            .put("sequence", sequence)
            .put("package_name", packageName)
            .put("window_title", windowTitle)
            .put("window_class", windowClass)
            .put("captured_at", capturedAt.toString())
            .put("nodes", encodedNodes)
    }
}

internal data class EventRecord(
    val sequence: Long,
    val kind: String,
    val packageName: String,
	val conversationKey: String,
    val conversation: String,
    val sender: String,
    val title: String,
    val text: String,
    val postedAt: Instant,
    val contentIntent: PendingIntent?,
) {
    fun toJson(): JSONObject = JSONObject()
        .put("sequence", sequence)
        .put("kind", kind)
        .put("package_name", packageName)
		.put("conversation_key", conversationKey)
        .put("conversation", conversation)
        .put("sender", sender)
        .put("title", title)
        .put("text", text)
		.put("openable", contentIntent != null)
        .put("posted_at", postedAt.toString())
}

internal data class CompanionAction(
    val kind: String,
    val nodeId: String,
    val text: String,
	val expectedSequence: Long,
)

internal data class ActionResult(
    val accepted: Boolean,
    val sequence: Long,
    val detail: String,
) {
    fun toJson(): JSONObject = JSONObject()
        .put("accepted", accepted)
        .put("sequence", sequence)
        .put("detail", detail)
}
