package com.jianxiang.link

import android.app.Activity
import android.app.AlertDialog
import android.content.Context
import android.content.Intent
import android.graphics.Color
import android.net.Uri
import android.os.Handler
import android.os.Looper
import android.view.Gravity
import android.view.View
import android.widget.LinearLayout
import android.widget.ProgressBar
import android.widget.TextView
import android.widget.Toast
import androidx.core.content.FileProvider
import org.json.JSONObject
import java.io.File
import java.io.FileOutputStream
import java.io.IOException
import java.security.MessageDigest
import java.security.SecureRandom
import java.util.Locale
import java.util.concurrent.CopyOnWriteArraySet
import java.util.concurrent.Executors
import javax.net.ssl.HostnameVerifier
import javax.net.ssl.HttpsURLConnection
import javax.net.ssl.SSLContext
import javax.net.ssl.X509TrustManager

/**
 * 检查更新（2026-11-19）：设置页手动检查 + 主页横幅提醒 + 下载 APK → 引导系统安装器安装。
 *
 * Android 不允许应用静默安装自己，所以只做标准路径：查清单 → 下载（进度/可后台/可取消）
 * → sha256 校验 → FileProvider + ACTION_VIEW 交给系统安装器（Android 8+ 用户需先授予
 * 「允许安装未知应用」，运行时引导）。
 *
 * 更新源是总端的平台化地址（服务端 jxaizz.com:2227，清单接口本轮先按约定实现：
 * `GET /updates/android/latest`，JSON 字段 platform/version/url/size/sha256/note/publishedAt），
 * 设置里可改（内测指向别的源）。壳连的是用户自己的电脑而非我们的服务器，所以更新
 * 信息只能来自这个内置源。
 *
 * 不打扰原则：没配源/网络不通/服务端没上该接口 → 一切照常，静默检查只记日志不弹窗，
 * 只有手动「检查更新」才给一句失败原因。
 */
object Update {
    /** 更新源 = 基地址 + 固定清单路径。用户在设置里只填基地址（要带 https:// 前缀），
     *  路径自动补上；误贴了 /updates/... 后缀也会收敛掉——与电脑端「公司服务器地址」
     *  的口径一致（内测换源只换 host:port，清单路径由两端约定）。 */
    const val DEFAULT_BASE = "https://jxaizz.com:2227"
    const val MANIFEST_PATH = "/updates/android/latest"
    private const val PREFS = "jxlink_update"
    private const val KEY_SOURCE = "source"
    private const val KEY_BANNER_OFF = "bannerOff"      // 被用户关掉横幅的版本号（同版本不再打扰）
    private const val KEY_LAST_FOUND = "lastFound"      // 最近查到「有更新」的清单 JSON（跨启动保留）
    private const val KEY_LAST_CHECK = "lastCheckAt"    // 静默检查节流
    private const val CHECK_INTERVAL = 60 * 60 * 1000L  // 静默检查 1 小时一次（与电脑端口径一致）

    private fun prefs(ctx: Context) = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)

    data class Info(
        val version: String,      // "2026xxxx-x"
        val url: String,          // APK 直链
        val size: Long,
        val sha256: String,
        val note: String,
        val publishedAt: String,
    ) {
        fun sizeText(): String = when {
            size >= 1024L * 1024 -> String.format(Locale.US, "%.1f MB", size / 1048576.0)
            size >= 1024L -> String.format(Locale.US, "%.0f KB", size / 1024.0)
            else -> "$size B"
        }

        fun toJSON(): String = JSONObject()
            .put("version", version).put("url", url).put("size", size)
            .put("sha256", sha256).put("note", note).put("publishedAt", publishedAt)
            .toString()

        companion object {
            fun fromJSON(raw: String): Info? = try {
                val o = JSONObject(raw)
                Info(
                    version = o.getString("version"), url = o.getString("url"),
                    size = o.optLong("size", 0L), sha256 = o.optString("sha256", ""),
                    note = o.optString("note", ""), publishedAt = o.optString("publishedAt", ""),
                )
            } catch (e: Exception) {
                null
            }
        }
    }

    sealed class Check {
        data class Ok(val info: Info, val newer: Boolean) : Check()
        data class Err(val msg: String) : Check()
    }

    /* ---------- 本地记忆 ---------- */

    /** 生效的更新源基地址（用户改过用改过的；脏值/带路径的旧值读时收敛） */
    fun source(ctx: Context): String {
        val saved = prefs(ctx).getString(KEY_SOURCE, null)?.takeIf { it.isNotBlank() } ?: return DEFAULT_BASE
        return normalizeBase(saved) ?: DEFAULT_BASE
    }

    /** 存自定义基地址（null/空 = 清掉自定义，回到默认） */
    fun setSource(ctx: Context, base: String?) {
        val v = base?.trim().orEmpty()
        prefs(ctx).edit().apply {
            if (v.isEmpty()) remove(KEY_SOURCE) else putString(KEY_SOURCE, v)
        }.apply()
    }

    /** 实际请求的清单地址：基地址 + 约定路径 */
    fun manifestUrl(ctx: Context): String = source(ctx) + MANIFEST_PATH

    /** 输入收敛成基地址：剥掉误贴的路径/尾斜杠，只留 scheme://host[:port]；
     *  不是 http(s) 开头或解析不出 host 返回 null（调用方提示） */
    fun normalizeBase(input: String): String? {
        val t = input.trim()
        if (!t.startsWith("http://") && !t.startsWith("https://")) return null
        return try {
            val u = java.net.URI(t)
            val host = u.host ?: return null
            val port = if (u.port > 0) ":${u.port}" else ""
            "${u.scheme}://$host$port"
        } catch (e: Exception) {
            null
        }
    }

    fun currentVersion(ctx: Context): String = try {
        ctx.packageManager.getPackageInfo(ctx.packageName, 0).versionName ?: "?"
    } catch (e: Exception) {
        "?"
    }

    fun bannerDismissed(ctx: Context): String? = prefs(ctx).getString(KEY_BANNER_OFF, null)

    fun dismissBanner(ctx: Context, version: String) {
        prefs(ctx).edit().putString(KEY_BANNER_OFF, version).apply()
    }

    /** 静默检查是否到期（节流；手动检查不受此限） */
    fun checkDue(ctx: Context): Boolean =
        System.currentTimeMillis() - prefs(ctx).getLong(KEY_LAST_CHECK, 0L) > CHECK_INTERVAL

    fun markChecked(ctx: Context) {
        prefs(ctx).edit().putLong(KEY_LAST_CHECK, System.currentTimeMillis()).apply()
    }

    /** 最近一次查到的「有更新」清单：离线重启也能提醒；装上/服务端不新了自动清 */
    fun lastFound(ctx: Context): Info? =
        prefs(ctx).getString(KEY_LAST_FOUND, null)?.let { Info.fromJSON(it) }

    fun setLastFound(ctx: Context, info: Info) {
        prefs(ctx).edit().putString(KEY_LAST_FOUND, info.toJSON()).apply()
    }

    fun clearLastFound(ctx: Context) {
        prefs(ctx).edit().remove(KEY_LAST_FOUND).apply()
    }

    /* ---------- 版本比较（与电脑端 update.go 的 versionCompare 同口径） ----------
       两套体系：新的「日期-序号」（20261115-1，兼容旧写法 20261008V10）与旧点分号
       （0.2.0）；日期体系恒新于旧体系；同为日期体系先比 8 位日期再比序号（-10 > -9）。 */
    private val dateVerRe = Regex("""^(\d{8})(?:[Vv]|-)(\d+)$""")

    private fun parseDateVersion(s: String): LongArray? {
        val m = dateVerRe.find(s.trim()) ?: return null
        val d = m.groupValues[1].toLongOrNull() ?: return null
        val n = m.groupValues[2].toLongOrNull() ?: return null
        return longArrayOf(d, n)
    }

    fun versionCompare(a: String, b: String): Int {
        val ad = parseDateVersion(a)
        val bd = parseDateVersion(b)
        if (ad != null && bd != null) {
            if (ad[0] != bd[0]) return if (ad[0] > bd[0]) 1 else -1
            if (ad[1] != bd[1]) return if (ad[1] > bd[1]) 1 else -1
            return 0
        }
        if (ad != null) return 1
        if (bd != null) return -1
        fun nums(s: String): IntArray {
            val parts = s.trim().removePrefix("v").split(".")
            return IntArray(3) { i ->
                parts.getOrNull(i)?.trim()?.toIntOrNull() ?: 0
            }
        }
        val an = nums(a)
        val bn = nums(b)
        for (i in 0 until 3) {
            if (an[i] != bn[i]) return if (an[i] > bn[i]) 1 else -1
        }
        return 0
    }

    /* ---------- 检查（阻塞，调用方上 IO 线程） ---------- */

    fun check(ctx: Context): Check {
        val src = manifestUrl(ctx)
        return try {
            val conn = openConn(src, "GET", 8000, 15000)
            when (val code = conn.responseCode) {
                200 -> {
                    val body = readCapped(conn, 256 * 1024)
                    val info = parseManifest(body)
                        ?: return Check.Err("清单格式不对（缺少 version/url 字段或平台不匹配）")
                    val newer = versionCompare(info.version, currentVersion(ctx)) > 0
                    if (newer) setLastFound(ctx, info) else clearLastFound(ctx)
                    Check.Ok(info, newer)
                }
                404 -> Check.Err("更新源没有提供 Android 清单（HTTP 404）")
                else -> Check.Err("HTTP $code")
            }
        } catch (e: Exception) {
            Check.Err(friendly(e))
        }
    }

    /** 约定的清单 JSON → Info；字段缺失/平台不符返回 null（服务端口径变了只改这里） */
    private fun parseManifest(body: String): Info? {
        val o = try {
            JSONObject(body)
        } catch (e: Exception) {
            return null
        }
        val platform = o.optString("platform", "")
        if (platform.isNotEmpty() && platform != "android") return null
        val ver = o.optString("version", o.optString("tag_name", ""))
        val url = o.optString("url", "")
        if (ver.isBlank() || url.isBlank()) return null
        return Info(
            version = ver.trim(), url = url.trim(), size = o.optLong("size", 0L),
            sha256 = o.optString("sha256", ""), note = o.optString("note", ""),
            publishedAt = o.optString("publishedAt", ""),
        )
    }

    private fun readCapped(conn: java.net.HttpURLConnection, cap: Int): String {
        val out = java.io.ByteArrayOutputStream()
        conn.inputStream.use { ins ->
            val buf = ByteArray(8192)
            while (true) {
                val n = ins.read(buf)
                if (n < 0) break
                out.write(buf, 0, n)
                if (out.size() > cap) throw IOException("清单过大（不像合法 JSON）")
            }
        }
        return out.toString("UTF-8")
    }

    fun friendly(e: Exception): String = when (e) {
        is java.net.UnknownHostException -> "域名解析失败（网络不可用或域名不对）"
        is java.net.ConnectException -> "连接不上更新源"
        is java.net.SocketTimeoutException -> "连接超时"
        is javax.net.ssl.SSLException -> "证书校验失败"
        is IOException -> e.message ?: "网络错误"
        else -> e.message ?: "未知错误"
    }

    /* ---------- HTTP 底座：证书策略与电脑端 tlspolicy.go 同口径 ----------
       域名与内网/回环 IP 放宽校验（自建服务拿不到公网证书，流量仍在 TLS 里），
       公网 IP 保持严格——与配对网关 onReceivedSslError 白名单同一取舍。 */
    private val trustAllFactory by lazy {
        val tm = object : X509TrustManager {
            override fun checkClientTrusted(chain: Array<java.security.cert.X509Certificate>?, authType: String?) {}
            override fun checkServerTrusted(chain: Array<java.security.cert.X509Certificate>?, authType: String?) {}
            override fun getAcceptedIssuers(): Array<java.security.cert.X509Certificate> = arrayOf()
        }
        SSLContext.getInstance("TLS").apply {
            init(null, arrayOf(tm), SecureRandom())
        }.socketFactory
    }

    internal fun relaxedTLSFor(host: String): Boolean {
        val h = host.trim().trim('[', ']').lowercase()
        if (h.isEmpty()) return false
        if (h.contains(':')) { // IPv6 字面量：回环/链路本地/唯一本地放宽
            return h == "::1" || h.startsWith("fe80:") || h.startsWith("fc") || h.startsWith("fd")
        }
        val m = Regex("""^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$""").find(h)
            ?: return true // 非 IP 字面量 → 域名：来源可信（内置源或用户手填），放宽
        val a = m.groupValues[1].toInt()
        val b = m.groupValues[2].toInt()
        return a == 10 || a == 127 || (a == 192 && b == 168) ||
            (a == 172 && b in 16..31) || (a == 169 && b == 254)
    }

    internal fun openConn(urlStr: String, method: String, connectMs: Int, readMs: Int): java.net.HttpURLConnection {
        val conn = java.net.URL(urlStr).openConnection() as java.net.HttpURLConnection
        if (conn is HttpsURLConnection && relaxedTLSFor(conn.url.host ?: "")) {
            conn.sslSocketFactory = trustAllFactory
            conn.hostnameVerifier = HostnameVerifier { _, _ -> true }
        }
        conn.requestMethod = method
        conn.connectTimeout = connectMs
        conn.readTimeout = readMs
        conn.setRequestProperty("User-Agent", "jx-link-update (Android)")
        return conn
    }

    /** 整文件算 sha256（已落盘文件复核用） */
    internal fun sha256Of(f: File): String {
        val dig = MessageDigest.getInstance("SHA-256")
        f.inputStream().use { ins ->
            val buf = ByteArray(65536)
            while (true) {
                val n = ins.read(buf)
                if (n < 0) break
                dig.update(buf, 0, n)
            }
        }
        return dig.digest().joinToString("") { "%02x".format(it) }
    }

    /* ---------- 安装引导（系统安装器；Android 不允许静默自装） ---------- */

    fun installOrGuide(act: Activity, file: File) {
        try {
            if (!act.packageManager.canRequestPackageInstalls()) {
                AlertDialog.Builder(act)
                    .setTitle("允许安装应用")
                    .setMessage(
                        "Android 8 起安装更新需要先授权本应用\n「允许安装未知应用」。\n\n" +
                            "点「去设置」打开授权页，打开开关后回来再点一次「安装」。",
                    )
                    .setPositiveButton("去设置") { _, _ ->
                        try {
                            act.startActivity(
                                Intent(
                                    android.provider.Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES,
                                    Uri.parse("package:" + act.packageName),
                                ),
                            )
                        } catch (e: Exception) {
                            Toast.makeText(act, "打不开授权页：${e.message}\n可在 系统设置·应用 里手动授权", Toast.LENGTH_LONG).show()
                        }
                    }
                    .setNegativeButton("取消", null)
                    .show()
                return
            }
            val uri = FileProvider.getUriForFile(act, act.packageName + ".fileprovider", file)
            val i = Intent(Intent.ACTION_VIEW).apply {
                setDataAndType(uri, "application/vnd.android.package-archive")
                addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
            }
            act.startActivity(i)
        } catch (e: Exception) {
            Toast.makeText(act, "无法启动安装：${e.message}", Toast.LENGTH_LONG).show()
        }
    }

    /* ---------- 下载/安装流程对话框（设置页与主页横幅共用） ---------- */

    /** 「发现新版本」流程框：下载进度 → 校验 → 安装按钮；可后台、可取消、可重试 */
    fun showFlow(act: Activity, info: Info) {
        fun dp(v: Float): Int = (v * act.resources.displayMetrics.density + 0.5f).toInt()

        val root = LinearLayout(act).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(dp(20f), dp(8f), dp(20f), dp(4f))
        }
        root.addView(
            TextView(act).apply {
                textSize = 13f
                setTextColor(Color.parseColor("#8A909C"))
                text = buildString {
                    append("大小：").append(info.sizeText())
                    if (info.publishedAt.isNotBlank()) append("　发布：").append(info.publishedAt.take(16))
                }
            },
        )
        if (info.note.isNotBlank()) {
            root.addView(
                TextView(act).apply {
                    textSize = 13f
                    setTextColor(Color.parseColor("#1F2329"))
                    text = info.note
                    maxLines = 6
                    setPadding(0, dp(8f), 0, 0)
                },
            )
        }
        val status = TextView(act).apply {
            textSize = 13f
            setTextColor(Color.parseColor("#1F2329"))
            setPadding(0, dp(10f), 0, dp(4f))
        }
        val bar = ProgressBar(act, null, android.R.attr.progressBarStyleHorizontal).apply {
            isIndeterminate = false
            max = 100
            progress = 0
        }
        root.addView(status)
        root.addView(
            bar,
            LinearLayout.LayoutParams(LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT),
        )

        val dialog = AlertDialog.Builder(act)
            .setTitle("发现新版本 ${info.version}")
            .setView(root)
            .setPositiveButton("下载并安装", null)
            .setNegativeButton("关闭", null)
            .create()

        fun wire(pos: String, posAct: () -> Unit, neg: String, negAct: () -> Unit) {
            dialog.getButton(AlertDialog.BUTTON_POSITIVE)?.apply {
                text = pos
                setOnClickListener { posAct() }
            }
            dialog.getButton(AlertDialog.BUTTON_NEGATIVE)?.apply {
                text = neg
                setOnClickListener { negAct() }
            }
        }

        fun render(s: UpdateDownload.State) {
            when (s) {
                is UpdateDownload.State.Idle -> {
                    status.text = "未开始下载"
                    bar.progress = 0
                    wire("下载并安装", { UpdateDownload.start(act, info) }, "关闭", { dialog.dismiss() })
                }
                is UpdateDownload.State.Running -> {
                    if (s.info.url != info.url) { // 状态属于别的版本（横幅过期等）→ 按未开始处理
                        status.text = "未开始下载"
                        bar.progress = 0
                        wire("下载并安装", { UpdateDownload.start(act, info) }, "关闭", { dialog.dismiss() })
                        return
                    }
                    status.text = if (s.total > 0)
                        "下载中… ${s.got * 100 / s.total}%（${s.got / 1048576} / ${s.total / 1048576} MB）"
                    else "下载中… 已下 ${s.got / 1048576} MB（大小未知）"
                    bar.progress = if (s.total > 0) (s.got * 100 / s.total).toInt() else 0
                    bar.isIndeterminate = s.total <= 0
                    wire("后台下载", { dialog.dismiss() }, "取消下载", { UpdateDownload.cancel() })
                }
                is UpdateDownload.State.Done -> {
                    if (s.info.url != info.url) {
                        status.text = "未开始下载"
                        bar.progress = 0
                        wire("下载并安装", { UpdateDownload.start(act, info) }, "关闭", { dialog.dismiss() })
                        return
                    }
                    status.text = "下载完成，校验通过 ✓\n点「安装」交给系统安装器完成升级"
                    bar.progress = 100
                    bar.isIndeterminate = false
                    wire("安装", { installOrGuide(act, s.file) }, "关闭", { dialog.dismiss() })
                }
                is UpdateDownload.State.Failed -> {
                    if (s.info.url != info.url) { // 旧版本/旧源的失败不挂到当前版本头上
                        status.text = "未开始下载"
                        bar.progress = 0
                        wire("下载并安装", { UpdateDownload.start(act, info) }, "关闭", { dialog.dismiss() })
                        return
                    }
                    status.text = "✕ ${s.msg}"
                    bar.progress = 0
                    bar.isIndeterminate = false
                    wire("重试下载", { UpdateDownload.start(act, info) }, "关闭", { dialog.dismiss() })
                }
            }
        }

        val un = UpdateDownload.listen { s -> if (!act.isDestroyed && dialog.isShowing) render(s) }
        dialog.setOnDismissListener { un() }
        dialog.show()
        render(UpdateDownload.state)
    }
}

/**
 * APK 下载状态机（单例）：主页横幅、设置页、流程对话框三处共享同一份状态，
 * 「后台下载」= 关掉对话框继续下，横幅上能看到进度与完成态。
 * 下载线程随进程走，壳被杀则作废（.part 下次启动清掉）——与 DownloadStore 同一取舍。
 */
object UpdateDownload {

    sealed class State {
        object Idle : State()
        data class Running(val info: Update.Info, val got: Long, val total: Long) : State()
        data class Done(val info: Update.Info, val file: File) : State()
        data class Failed(val info: Update.Info, val msg: String) : State()
    }

    @Volatile
    var state: State = State.Idle
        private set

    private val main = Handler(Looper.getMainLooper())
    private val io = Executors.newSingleThreadExecutor()
    private val listeners = CopyOnWriteArraySet<(State) -> Unit>()
    @Volatile
    private var cancelled = false

    private class Cancelled : Exception()

    /** 订阅状态变化（回调已切主线程）；返回退订函数 */
    fun listen(cb: (State) -> Unit): () -> Unit {
        listeners.add(cb)
        return { listeners.remove(cb) }
    }

    private fun set(s: State) {
        state = s
        main.post { listeners.forEach { it(s) } }
    }

    fun dir(ctx: Context): File = File(ctx.filesDir, "updates").apply { mkdirs() }

    fun fileFor(ctx: Context, version: String): File =
        File(dir(ctx), "jx-link-" + version.replace(Regex("[^A-Za-z0-9._-]"), "_") + ".apk")

    fun start(ctx: Context, info: Update.Info) {
        val cur = state
        if (cur is State.Running && cur.info.url == info.url) return // 同一个已在下
        val appCtx = ctx.applicationContext
        cancelled = false
        set(State.Running(info, 0L, info.size))
        io.execute { run(appCtx, info) }
    }

    fun cancel() {
        cancelled = true // run() 感知后收尾置 Idle
    }

    private fun run(ctx: Context, info: Update.Info) {
        val finalFile = fileFor(ctx, info.version)
        val part = File(finalFile.parentFile, finalFile.name + ".part")
        try {
            // 上次已下完且校验通过 → 直接 Done（省流量；Done 态进对话框即可安装）
            if (finalFile.exists() && shaOK(finalFile, info.sha256)) {
                set(State.Done(info, finalFile))
                return
            }
            // 清掉旧版本的包与残留 .part（目录里只留当前这一个）
            dir(ctx).listFiles()?.forEach {
                if (it != finalFile) it.delete()
            }
            val conn = Update.openConn(info.url, "GET", 10000, 30000)
            if (conn.responseCode !in 200..299) throw IOException("HTTP ${conn.responseCode}")
            val total = if (info.size > 0) info.size
            else (conn.contentLengthLong.takeIf { it > 0 } ?: 0L)
            val dig = MessageDigest.getInstance("SHA-256")
            var got = 0L
            var lastPost = 0L
            conn.inputStream.use { ins ->
                FileOutputStream(part).use { out ->
                    val buf = ByteArray(65536)
                    while (true) {
                        if (cancelled) throw Cancelled()
                        val n = ins.read(buf)
                        if (n < 0) break
                        out.write(buf, 0, n)
                        dig.update(buf, 0, n)
                        got += n
                        if (got - lastPost >= 128 * 1024) { // 进度节流：每 128KB 刷一次
                            lastPost = got
                            set(State.Running(info, got, total))
                        }
                    }
                    out.flush()
                }
            }
            if (cancelled) throw Cancelled()
            val expect = info.sha256.trim().lowercase()
            if (expect.isNotEmpty()) {
                val sha = dig.digest().joinToString("") { "%02x".format(it) }
                if (sha != expect) {
                    part.delete()
                    set(State.Failed(info, "安装包校验不一致（SHA-256 不匹配），已删除损坏文件；请重试下载"))
                    return
                }
            }
            if (finalFile.exists()) finalFile.delete()
            if (!part.renameTo(finalFile)) throw IOException("保存安装包失败")
            set(State.Done(info, finalFile))
        } catch (e: Exception) {
            part.delete()
            set(if (e is Cancelled) State.Idle else State.Failed(info, "下载失败：${Update.friendly(e)}"))
        }
    }

    private fun shaOK(f: File, expect: String): Boolean {
        val e = expect.trim().lowercase()
        if (e.isEmpty()) return true // 清单没给 sha 就不拦（弱化但不误伤）
        return Update.sha256Of(f) == e
    }
}
