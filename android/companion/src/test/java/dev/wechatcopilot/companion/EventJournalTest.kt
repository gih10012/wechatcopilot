package dev.wechatcopilot.companion

import java.time.Instant
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertTrue

class EventJournalTest {
    @Test
    fun conversationBoundaryMovesWhenTheGlobalRingEvictsItsOldestRecord() {
        val journal = BoundedEventJournal(4)
        journal.add(event(1, "conversation-a"))
        journal.add(event(2, "conversation-b"))
        journal.add(event(3, "conversation-a"))
        journal.add(event(4, "conversation-b"))

        val initial = journal.after(0, 500)
        assertEquals(listOf(true, true, false, false), initial.map { it.gapBefore })

        // Evicting A1 must move A's boundary to A3 even though B2 remains the
        // first record in the global journal.
        journal.add(event(5, "conversation-c"))
        val retained = journal.after(0, 500)
        assertEquals(listOf(2L, 3L, 4L, 5L), retained.map { it.sequence })
        assertEquals(
            listOf(3L),
            retained.filter { it.conversationKey == "conversation-a" && it.gapBefore }
                .map { it.sequence },
        )
        assertEquals(
            listOf(2L),
            retained.filter { it.conversationKey == "conversation-b" && it.gapBefore }
                .map { it.sequence },
        )
        assertTrue(retained.single { it.sequence == 5L }.gapBefore)

        // A page that starts after B's boundary still carries A's independent
        // boundary, so host-side conversation filtering cannot lose it.
        val laterPageForA = journal.after(2, 500).filter { it.conversationKey == "conversation-a" }
        assertEquals(1, laterPageForA.size)
        assertTrue(laterPageForA.single().gapBefore)
    }

    @Test
    fun onlyTheOldestRetainedRecordPerConversationCarriesTheBoundary() {
        val journal = BoundedEventJournal(2)
        journal.add(event(10, "conversation-a"))
        journal.add(event(11, "conversation-a"))

        var retained = journal.after(0, 500)
        assertTrue(retained[0].gapBefore)
        assertFalse(retained[1].gapBefore)

        journal.add(event(12, "conversation-a"))
        retained = journal.after(0, 500)
        assertEquals(listOf(11L, 12L), retained.map { it.sequence })
        assertTrue(retained[0].gapBefore)
        assertFalse(retained[1].gapBefore)
    }

    private fun event(sequence: Long, conversationKey: String) = EventRecord(
        sequence = sequence,
        kind = "notification",
        packageName = CompanionRuntime.WECOM_PACKAGE,
        conversationKey = conversationKey,
        conversation = conversationKey,
        sender = "sender",
        title = "title",
        text = "message-$sequence",
        postedAt = Instant.ofEpochSecond(sequence),
        contentIntent = null,
    )
}
