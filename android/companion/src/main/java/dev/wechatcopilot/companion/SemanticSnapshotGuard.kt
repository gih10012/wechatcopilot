package dev.wechatcopilot.companion

internal data class SemanticTreeFingerprint(
    val packageName: String,
    val windowId: Int,
    val windowTitle: String,
    val windowClass: String,
    val complete: Boolean,
    val nodes: List<UiNodeModel>,
)

internal class SemanticSnapshotGuard {
    private data class RecordedSnapshot(
        val sequence: Long,
        val fingerprint: SemanticTreeFingerprint,
    )

    private var lastObserved: RecordedSnapshot? = null
    private var actionable: RecordedSnapshot? = null
    private var consumedFingerprint: SemanticTreeFingerprint? = null

    fun needsSequenceAdvance(sequence: Long, fingerprint: SemanticTreeFingerprint): Boolean {
        val previous = lastObserved
        return previous != null && previous.sequence == sequence && previous.fingerprint != fingerprint
    }

    fun record(sequence: Long, fingerprint: SemanticTreeFingerprint) {
        val snapshot = RecordedSnapshot(sequence, fingerprint)
        lastObserved = snapshot
        if (!fingerprint.complete || fingerprint == consumedFingerprint) {
            actionable = null
            return
        }
        consumedFingerprint = null
        actionable = snapshot
    }

    fun matches(
        expectedSequence: Long,
        currentSequence: Long,
        fingerprint: SemanticTreeFingerprint,
    ): Boolean {
        if (expectedSequence <= 0 || expectedSequence != currentSequence || !fingerprint.complete) return false
        val snapshot = actionable ?: return false
        return snapshot.sequence == expectedSequence && snapshot.fingerprint == fingerprint
    }

    fun consume(
        expectedSequence: Long,
        currentSequence: Long,
        fingerprint: SemanticTreeFingerprint,
    ): Boolean {
        if (!matches(expectedSequence, currentSequence, fingerprint)) return false
        actionable = null
        consumedFingerprint = fingerprint
        return true
    }
}
