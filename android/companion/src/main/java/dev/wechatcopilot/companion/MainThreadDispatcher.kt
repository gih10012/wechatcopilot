package dev.wechatcopilot.companion

import android.os.Handler
import android.os.Looper
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

internal sealed interface DispatchOutcome<out T> {
    data class Completed<T>(val value: T) : DispatchOutcome<T>
    data class Failed(val cause: Throwable) : DispatchOutcome<Nothing>
    data object CancelledBeforeStart : DispatchOutcome<Nothing>
    data object RunningAtDeadline : DispatchOutcome<Nothing>
    data object SchedulerRejected : DispatchOutcome<Nothing>
}

internal interface MainThreadScheduler {
    fun isMainThread(): Boolean
    fun post(runnable: Runnable): Boolean
    fun remove(runnable: Runnable)
}

internal class AndroidMainThreadScheduler(
    private val handler: Handler = Handler(Looper.getMainLooper()),
) : MainThreadScheduler {
    override fun isMainThread(): Boolean = Looper.myLooper() == handler.looper

    override fun post(runnable: Runnable): Boolean = handler.post(runnable)

    override fun remove(runnable: Runnable) = handler.removeCallbacks(runnable)
}

internal class MainThreadDispatcher(
    private val scheduler: MainThreadScheduler,
    private val timeoutMillis: Long,
) {
    init {
        require(timeoutMillis > 0) { "main-thread dispatch timeout must be positive" }
    }

    fun <T> dispatch(block: () -> T): DispatchOutcome<T> {
        if (scheduler.isMainThread()) return runInline(block)

        val call = PendingMainThreadCall(block)
        if (!scheduler.post(call)) return DispatchOutcome.SchedulerRejected
        try {
            if (call.await(timeoutMillis)) return call.completedOutcome()
        } catch (_: InterruptedException) {
            Thread.currentThread().interrupt()
        }

        return call.resolveDeadline().also { outcome ->
            if (outcome === DispatchOutcome.CancelledBeforeStart) scheduler.remove(call)
        }
    }

    private fun <T> runInline(block: () -> T): DispatchOutcome<T> = try {
        DispatchOutcome.Completed(block())
    } catch (error: Throwable) {
        DispatchOutcome.Failed(error)
    }
}

internal class PendingMainThreadCall<T>(
    private val block: () -> T,
) : Runnable {
    private enum class State {
        QUEUED,
        RUNNING,
        COMPLETED,
        CANCELLED,
    }

    private val completion = CountDownLatch(1)
    private var state = State.QUEUED
    private var outcome: DispatchOutcome<T>? = null

    override fun run() {
        synchronized(this) {
            if (state != State.QUEUED) return
            state = State.RUNNING
        }
        val completed = try {
            DispatchOutcome.Completed(block())
        } catch (error: Throwable) {
            DispatchOutcome.Failed(error)
        }
        synchronized(this) {
            outcome = completed
            state = State.COMPLETED
        }
        completion.countDown()
    }

    fun await(timeoutMillis: Long): Boolean = completion.await(timeoutMillis, TimeUnit.MILLISECONDS)

    @Synchronized
    fun completedOutcome(): DispatchOutcome<T> =
        outcome ?: error("main-thread call signaled without an outcome")

    @Synchronized
    fun resolveDeadline(): DispatchOutcome<T> = when (state) {
        State.QUEUED -> {
            state = State.CANCELLED
            completion.countDown()
            DispatchOutcome.CancelledBeforeStart
        }
        State.RUNNING -> DispatchOutcome.RunningAtDeadline
        State.COMPLETED -> outcome ?: error("completed main-thread call has no outcome")
        State.CANCELLED -> DispatchOutcome.CancelledBeforeStart
    }
}
