package online.timefiles.vantaloom

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.os.Build
import androidx.core.app.NotificationCompat
import org.json.JSONObject
import java.util.concurrent.atomic.AtomicInteger

/**
 * 运行时消息通知（0.14.31）：子进程没有 NotificationManager，后端 notify 包在
 * Android 构建下把每条通知打成 stdout 单行标记（@@VANTALOOM-NOTIFY@@{json}），
 * 由 LocalRuntime 的日志泵截获后在这里发真正的系统通知。WebView 睡眠也照发
 * （前台服务保活进程）；「人正在看应用」的抑制由后端 ui-presence 焦点窗口负
 * 责，壳侧不重复判断。POST_NOTIFICATIONS 未授权时 notify 被系统静默丢弃，
 * 不抛错。
 */
object RuntimeNotifications {
    const val MARKER = "@@VANTALOOM-NOTIFY@@"

    /**
     * 点击目标（0.15.14）：标记行可带 conversationId/machineId，随点击 Intent
     * 进 MainActivity → 前端打开对应对话。缺省 = 只把应用拉到前台（旧行为）。
     */
    const val EXTRA_CONVERSATION_ID = "vtl.notify.conversationId"
    const val EXTRA_MACHINE_ID = "vtl.notify.machineId"

    private const val CHANNEL_ID = "vantaloom_messages"
    private val nextId = AtomicInteger(2000)
    @Volatile private var channelReady = false

    /** 日志泵每行喂进来；是标记行则消费并返回 true（不再进 logcat）。 */
    fun handleMarkerLine(context: Context, line: String): Boolean {
        if (!line.startsWith(MARKER)) return false
        runCatching {
            val payload = JSONObject(line.substring(MARKER.length))
            post(
                context,
                payload.optString("title").ifBlank { "Vantaloom" },
                payload.optString("body"),
                payload.optString("conversationId").trim(),
                payload.optString("machineId").trim(),
            )
        }
        return true
    }

    private fun post(
        context: Context,
        title: String,
        body: String,
        conversationId: String = "",
        machineId: String = "",
    ) {
        val manager =
            context.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O && !channelReady) {
            manager.createNotificationChannel(
                NotificationChannel(
                    CHANNEL_ID,
                    context.getString(R.string.messages_channel_name),
                    NotificationManager.IMPORTANCE_DEFAULT,
                ),
            )
            channelReady = true
        }
        val notificationId = nextId.getAndIncrement()
        val tapIntent = Intent(context, MainActivity::class.java)
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        if (conversationId.isNotEmpty()) {
            tapIntent.putExtra(EXTRA_CONVERSATION_ID, conversationId)
            if (machineId.isNotEmpty()) tapIntent.putExtra(EXTRA_MACHINE_ID, machineId)
            // 不同通知必须有不同的 requestCode：PendingIntent 的等价性不看 extras，
            // 固定 0 + FLAG_UPDATE_CURRENT 会让所有待处理通知共享**同一份** extras
            // （点旧通知跳到新对话）。用通知 id 当 requestCode 天然唯一。
            tapIntent.setAction("$ACTION_OPEN_CONVERSATION.$notificationId")
        }
        val tap = PendingIntent.getActivity(
            context,
            notificationId,
            tapIntent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
        val notification = NotificationCompat.Builder(context, CHANNEL_ID)
            .setSmallIcon(android.R.drawable.stat_notify_chat)
            .setContentTitle(title)
            .setContentText(body)
            .setStyle(NotificationCompat.BigTextStyle().bigText(body))
            .setContentIntent(tap)
            .setAutoCancel(true)
            .setPriority(NotificationCompat.PRIORITY_DEFAULT)
            .build()
        runCatching { manager.notify(notificationId, notification) }
    }

    private const val ACTION_OPEN_CONVERSATION = "online.timefiles.vantaloom.OPEN_CONVERSATION"
}
