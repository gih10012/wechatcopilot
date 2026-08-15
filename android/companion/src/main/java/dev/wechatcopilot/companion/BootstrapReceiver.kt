package dev.wechatcopilot.companion

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

class BootstrapReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != "dev.wechatcopilot.companion.BOOTSTRAP") return
		CompanionRuntime.start(context)
    }
}
