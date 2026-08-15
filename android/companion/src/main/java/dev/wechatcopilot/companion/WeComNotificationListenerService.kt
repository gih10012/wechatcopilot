package dev.wechatcopilot.companion

import android.app.Notification
import android.service.notification.NotificationListenerService
import android.service.notification.StatusBarNotification
import java.security.MessageDigest
import java.time.Instant

class WeComNotificationListenerService : NotificationListenerService() {
    override fun onListenerConnected() {
        super.onListenerConnected()
        CompanionRuntime.start(this)
    }

    override fun onNotificationPosted(notification: StatusBarNotification?) {
        val item = notification ?: return
        if (item.packageName != CompanionRuntime.WECOM_PACKAGE) return
        val extras = item.notification.extras
        val title = extras.getCharSequence(Notification.EXTRA_TITLE)?.toString().orEmpty()
        val text = extras.getCharSequence(Notification.EXTRA_TEXT)?.toString().orEmpty()
        val conversation = extras.getCharSequence(Notification.EXTRA_CONVERSATION_TITLE)?.toString().orEmpty()
            .ifBlank { title }
        val sender = extras.getCharSequence(Notification.EXTRA_TITLE_BIG)?.toString().orEmpty()
            .ifBlank { title }
        CompanionRuntime.addNotification(
            packageName = item.packageName,
			conversationKey = stableConversationKey(item),
            conversation = conversation,
            sender = sender,
            title = title,
            text = text,
            postedAt = Instant.ofEpochMilli(item.postTime),
            contentIntent = item.notification.contentIntent,
        )
    }

	private fun stableConversationKey(item: StatusBarNotification): String {
		val shortcut = item.notification.shortcutId?.takeIf { it.isNotBlank() }
		val source = if (shortcut != null) {
			"shortcut:${item.packageName}:$shortcut"
		} else {
			"notification:${item.key}"
		}
		return MessageDigest.getInstance("SHA-256")
			.digest(source.toByteArray(Charsets.UTF_8))
			.joinToString("") { "%02x".format(it) }
	}
}
