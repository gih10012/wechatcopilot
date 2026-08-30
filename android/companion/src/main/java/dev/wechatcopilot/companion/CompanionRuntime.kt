package dev.wechatcopilot.companion

import android.app.PendingIntent
import android.content.Context
import android.util.Log
import java.io.File
import java.security.SecureRandom
import java.security.MessageDigest
import java.time.Instant
import java.util.ArrayDeque
import java.util.Base64
import java.util.concurrent.atomic.AtomicLong
import java.util.concurrent.atomic.AtomicReference

internal object CompanionRuntime {
    const val PORT = 18765
    const val WECOM_PACKAGE = "com.tencent.wework"
    private const val TOKEN_FILE = "rpc-token"
    private const val MAX_EVENTS = 2_000

    // A time-based epoch keeps host cursors monotonic across companion process restarts.
    private val sequence = AtomicLong(System.currentTimeMillis() * 1_000L)
    private val accessibility = AtomicReference<WeComAccessibilityService?>()
    private val observedWindow = AtomicReference<ObservedWindow?>()
    private val events = BoundedEventJournal(MAX_EVENTS)
    private val server = AtomicReference<LocalRpcServer?>()

    fun start(context: Context) {
        if (server.get() != null) return
        synchronized(server) {
            if (server.get() != null) return
            try {
				ensureToken(context.applicationContext)
                val instance = LocalRpcServer(context.applicationContext, PORT)
                instance.start()
                server.set(instance)
            } catch (error: Exception) {
                Log.e("WCCCompanion", "Unable to bind loopback RPC listener", error)
            }
        }
    }

    fun attach(service: WeComAccessibilityService) {
        observedWindow.set(null)
        accessibility.set(service)
        start(service)
        markUiChanged()
    }

    fun detach(service: WeComAccessibilityService) {
        if (accessibility.compareAndSet(service, null)) {
            observedWindow.set(null)
            markUiChanged()
        }
    }

    fun markUiChanged(): Long = sequence.incrementAndGet()

    fun observeWindow(className: String, windowId: Int): Long {
        observedWindow.set(className.takeIf { it.isNotBlank() }?.let { ObservedWindow(it, windowId) })
        return markUiChanged()
    }

    fun currentSequence(): Long = sequence.get()

    fun advanceSequenceIfCurrent(expected: Long): Long? {
        if (expected <= 0 || expected == Long.MAX_VALUE) return null
        val advanced = expected + 1
        return advanced.takeIf { sequence.compareAndSet(expected, advanced) }
    }

    fun isAttached(service: WeComAccessibilityService): Boolean = accessibility.get() === service

    fun currentWindowClass(packageName: String, windowId: Int): String {
        if (packageName != WECOM_PACKAGE || windowId < 0) return ""
        val observed = observedWindow.get() ?: return ""
        return observed.className.takeIf { observed.windowId >= 0 && observed.windowId == windowId }.orEmpty()
    }

    fun snapshot(): UiSnapshotModel = accessibility.get()?.buildSnapshot()
        ?: UiSnapshotModel(currentSequence(), "", -1, "", "", Instant.now(), emptyList())

    fun perform(action: CompanionAction): ActionResult {
        return when (action.kind) {
            "open_notification" -> openNotification(action.nodeId)
            else -> accessibility.get()?.performConstrainedAction(action)
                ?: ActionResult(false, currentSequence(), "accessibility service is not connected")
        }
    }

    fun addNotification(
        packageName: String,
        conversationKey: String,
        conversation: String,
        sender: String,
        title: String,
        text: String,
        postedAt: Instant,
        contentIntent: PendingIntent?,
    ) {
        val event = EventRecord(
            sequence = sequence.incrementAndGet(),
            kind = "notification",
            packageName = packageName,
            conversationKey = conversationKey,
            conversation = conversation,
            sender = sender,
            title = title,
            text = text,
            postedAt = postedAt,
            contentIntent = contentIntent,
        )
        events.add(event)
    }

    fun eventsAfter(after: Long, limit: Int): List<EventRecord> = events.after(after, limit)

    fun oldestEventSequence(): Long? = events.oldestSequence()

    fun tokenMatches(context: Context, provided: String): Boolean {
		val expected = try {
			ensureToken(context)
		} catch (_: Exception) {
			return false
		}
        return MessageDigest.isEqual(expected.toByteArray(Charsets.UTF_8), provided.toByteArray(Charsets.UTF_8))
    }

	private fun ensureToken(context: Context): String {
		val target = File(context.filesDir, TOKEN_FILE)
		if (target.exists()) {
			val existing = target.readText(Charsets.US_ASCII).trim()
			check(TokenPolicy.isValid(existing)) { "persisted RPC token is invalid" }
			return existing
		}
		val bytes = ByteArray(32).also { SecureRandom().nextBytes(it) }
		val token = Base64.getUrlEncoder().withoutPadding().encodeToString(bytes)
		check(TokenPolicy.isValid(token))
		val temporary = File.createTempFile(".rpc-token-", ".tmp", context.filesDir)
		try {
			temporary.writeText("$token\n", Charsets.US_ASCII)
			temporary.setReadable(false, false)
			temporary.setWritable(false, false)
			check(temporary.setReadable(true, true) && temporary.setWritable(true, true)) {
				"cannot protect RPC token"
			}
			check(temporary.renameTo(target)) { "cannot persist RPC token" }
		} finally {
			temporary.delete()
		}
		return token
	}

    private fun openNotification(sequenceText: String): ActionResult {
        val target = sequenceText.toLongOrNull()
            ?: return ActionResult(false, currentSequence(), "notification sequence is invalid")
        val event = events.find(target)
            ?: return ActionResult(false, currentSequence(), "notification is no longer in the bounded event journal")
        val intent = event.contentIntent
            ?: return ActionResult(false, currentSequence(), "notification has no content intent")
        return try {
            intent.send()
            ActionResult(true, currentSequence(), "notification opened")
        } catch (error: PendingIntent.CanceledException) {
            Log.w("WCCCompanion", "Notification intent expired", error)
            ActionResult(false, currentSequence(), "notification intent expired")
        }
    }
}

/**
 * A conversation-scoped bounded journal.
 *
 * The first currently retained record for every conversation is marked as a
 * boundary. When that record is evicted, the marker moves to the next retained
 * record for the same conversation. This makes a global ring truncation remain
 * visible after the host filters or persists messages by conversation.
 */
internal class BoundedEventJournal(private val capacity: Int) {
    private val records = ArrayDeque<EventRecord>()

    init {
        require(capacity > 0) { "event journal capacity must be positive" }
    }

    fun add(record: EventRecord) = synchronized(records) {
        val conversationRetained = records.any { it.conversationKey == record.conversationKey }
        records.addLast(record.copy(gapBefore = record.gapBefore || !conversationRetained))
        while (records.size > capacity) {
            val removed = records.removeFirst()
            markOldestConversationRecord(removed.conversationKey)
        }
    }

    fun after(after: Long, limit: Int): List<EventRecord> = synchronized(records) {
        records.asSequence()
            .filter { it.sequence > after }
            .take(limit.coerceIn(1, 500))
            .toList()
    }

    fun oldestSequence(): Long? = synchronized(records) { records.firstOrNull()?.sequence }

    fun find(sequence: Long): EventRecord? = synchronized(records) {
        records.firstOrNull { it.sequence == sequence }
    }

    private fun markOldestConversationRecord(conversationKey: String) {
        val replacement = ArrayDeque<EventRecord>(records.size)
        var marked = false
        records.forEach { record ->
            if (!marked && record.conversationKey == conversationKey) {
                replacement.addLast(record.copy(gapBefore = true))
                marked = true
            } else {
                replacement.addLast(record)
            }
        }
        if (marked) {
            records.clear()
            records.addAll(replacement)
        }
    }
}

private data class ObservedWindow(val className: String, val windowId: Int)

internal object TokenPolicy {
    private val tokenPattern = Regex("^[A-Za-z0-9_-]{43,128}$")

    fun isValid(token: String): Boolean = tokenPattern.matches(token)
}
