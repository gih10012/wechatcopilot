package dev.wechatcopilot.companion

import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertIs
import kotlin.test.assertTrue

class AtomicActionTest {
    @Test
    fun queuedDispatchIsCancelledBeforeItCanMutate() {
        val scheduler = HoldingScheduler()
        val invoked = AtomicBoolean(false)
        val outcome = MainThreadDispatcher(scheduler, 1).dispatch {
            invoked.set(true)
        }

        assertEquals(DispatchOutcome.CancelledBeforeStart, outcome)
        assertTrue(scheduler.removed)
        scheduler.queued?.run()
        assertFalse(invoked.get())
    }

    @Test
    fun runningDispatchAtDeadlineIsReportedAsUncertain() {
        val started = CountDownLatch(1)
        val release = CountDownLatch(1)
        val finished = CountDownLatch(1)
        val scheduler = ThreadScheduler(finished)

        val outcome = MainThreadDispatcher(scheduler, 250).dispatch {
            started.countDown()
            release.await()
            "done"
        }

        assertTrue(started.await(1, TimeUnit.SECONDS))
        assertEquals(DispatchOutcome.RunningAtDeadline, outcome)
        release.countDown()
        assertTrue(finished.await(1, TimeUnit.SECONDS))
    }

    @Test
    fun inlineDispatchCapturesExceptions() {
        val scheduler = object : MainThreadScheduler {
            override fun isMainThread() = true
            override fun post(runnable: Runnable) = error("post must not be called")
            override fun remove(runnable: Runnable) = Unit
        }
        val outcome = MainThreadDispatcher(scheduler, 100).dispatch<Unit> {
            throw IllegalStateException("boom")
        }

        assertIs<DispatchOutcome.Failed>(outcome)
        assertIs<IllegalStateException>(outcome.cause)
    }

    @Test
    fun semanticFingerprintMustMatchEveryCapturedField() {
        val guard = SemanticSnapshotGuard()
        val original = fingerprint()
        guard.record(41, original)

        assertTrue(guard.matches(41, 41, original))
        assertFalse(guard.matches(40, 41, original))
        assertFalse(guard.matches(41, 42, original))
        assertFalse(guard.matches(41, 41, original.copy(windowId = 8)))
        assertFalse(guard.matches(41, 41, original.copy(windowClass = "other.Activity")))
        assertFalse(guard.matches(41, 41, original.copy(windowTitle = "Other")))
        assertFalse(guard.matches(41, 41, original.copy(packageName = "other.package")))
        assertFalse(guard.matches(41, 41, original.copy(complete = false)))
        assertFalse(
            guard.matches(
                41,
                41,
                original.copy(nodes = listOf(original.nodes.single().copy(checked = true))),
            ),
        )
        assertFalse(
            guard.matches(
                41,
                41,
                original.copy(nodes = listOf(original.nodes.single().copy(selected = true))),
            ),
        )
    }

    @Test
    fun semanticSnapshotIsSingleUseUntilTheTreeChanges() {
        val guard = SemanticSnapshotGuard()
        val original = fingerprint()
        guard.record(41, original)

        assertTrue(guard.consume(41, 41, original))
        assertFalse(guard.consume(41, 41, original))
        assertFalse(guard.matches(41, 41, original))

        // A sequence-only event cannot make the already-invoked tree actionable again.
        guard.record(42, original)
        assertFalse(guard.matches(42, 42, original))

        val changed = original.copy(nodes = listOf(original.nodes.single().copy(checked = true)))
        guard.record(42, changed)
        assertTrue(guard.matches(42, 42, changed))
    }

    @Test
    fun globalBackRequiresTheSameOfficialWindowAndConsumesTheSnapshot() {
        val guard = SemanticSnapshotGuard()
        val original = fingerprint()
        guard.record(41, original)

        assertFalse(
            consumeConstrainedGlobalBack(
                guard,
                41,
                41,
                original,
                original.copy(windowId = original.windowId + 1),
            ),
        )
        assertTrue(consumeConstrainedGlobalBack(guard, 41, 41, original, original))
        assertFalse(consumeConstrainedGlobalBack(guard, 41, 41, original, original))
    }

    @Test
    fun unannouncedTreeDriftRequiresANewSequence() {
        val guard = SemanticSnapshotGuard()
        val original = fingerprint()
        val changed = original.copy(nodes = listOf(original.nodes.single().copy(text = "Changed")))
        guard.record(41, original)

        assertFalse(guard.needsSequenceAdvance(41, original))
        assertTrue(guard.needsSequenceAdvance(41, changed))
        assertFalse(guard.needsSequenceAdvance(42, changed))
    }

    private fun fingerprint(): SemanticTreeFingerprint = SemanticTreeFingerprint(
        packageName = CompanionRuntime.WECOM_PACKAGE,
        windowId = 7,
        windowTitle = "WeCom",
        windowClass = "com.tencent.wework.login.controller.LoginWxAuthActivity",
        complete = true,
        nodes = listOf(
            UiNodeModel(
                id = "0/1",
                parentId = "0",
                className = "android.widget.CheckBox",
                viewId = "terms",
                text = "Read and Agree",
                contentDescription = "",
                bounds = BoundsModel(10, 20, 100, 60),
                clickable = true,
                checkable = true,
                checked = false,
                selected = false,
                editable = false,
                scrollable = false,
                enabled = true,
                focused = false,
                visibleToUser = true,
            ),
        ),
    )

    private class HoldingScheduler : MainThreadScheduler {
        var queued: Runnable? = null
        var removed = false

        override fun isMainThread() = false

        override fun post(runnable: Runnable): Boolean {
            queued = runnable
            return true
        }

        override fun remove(runnable: Runnable) {
            removed = queued === runnable
        }
    }

    private class ThreadScheduler(
        private val finished: CountDownLatch,
    ) : MainThreadScheduler {
        override fun isMainThread() = false

        override fun post(runnable: Runnable): Boolean {
            Thread {
                try {
                    runnable.run()
                } finally {
                    finished.countDown()
                }
            }.start()
            return true
        }

        override fun remove(runnable: Runnable) = Unit
    }
}
