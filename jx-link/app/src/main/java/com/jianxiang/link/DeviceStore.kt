package com.jianxiang.link

import android.annotation.SuppressLint
import android.content.Context
import android.os.Handler
import android.os.Looper
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebSettings
import android.webkit.WebStorage
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.webkit.Profile
import androidx.webkit.ProfileStore
import androidx.webkit.WebViewFeature
import org.json.JSONArray
import org.json.JSONObject
import java.security.MessageDigest
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

/**
 * 一台电脑（连接目标）。
 * - relay：来自入口二维码 http://网关:端口/a/设备号#pk=配对密钥（与电脑端 设置·网络访问 同源）
 * - lan：局域网直连 http://ip:端口（电脑端开启「局域网访问」后的地址）
 */
data class Dev(
    val id: String,        // 唯一键：relay=设备号；lan=host:port
    val name: String,      // 显示名（默认=设备号 / host）
    val kind: String,      // "relay" | "lan"
    val scheme: String,    // http / https
    val host: String,
    val port: Int,
    val pairKey: String,   // 仅 relay
    val url: String,       // 仅 lan（完整入口）
    val addedAt: Long,
    val alias: String = "", // 用户备注名（长按菜单「备注」设置；空 = 未设置）
) {
    /** 统一显示名：备注优先（列表、菜单、设备页、分享选择都用它） */
    fun displayName(): String = alias.ifBlank { name }

    /** 入口 URL：每次都带 #pk（localStorage 丢失也能自愈直连） */
    fun entryUrl(): String {
        if (kind == "lan") return url
        return "$scheme://$host:$port/a/$id#pk=$pairKey"
    }

    /** 本机独立 WebView Profile 名：同设备恒定，只留安全字符 + 短哈希防碰撞 */
    fun profileName(): String {
        val safe = id.replace(Regex("[^A-Za-z0-9_-]"), "_").take(40)
        val dig = MessageDigest.getInstance("SHA-1").digest("$kind/$id".toByteArray())
        val h = dig.joinToString("") { "%02x".format(it) }.slice(0 until 8)
        return "dev-$safe-$h"
    }
}

/** 设备列表 + 「上次所在电脑」记忆，SharedPreferences 落 JSON（设备量级个位数，无需数据库） */
object DeviceStore {
    private const val PREFS = "jxlink"
    private const val KEY_DEVS = "devices"
    private const val KEY_LAST = "lastDevice"

    @SuppressLint("MutatingSharedPreferences")
    fun load(ctx: Context): MutableList<Dev> {
        val raw = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getString(KEY_DEVS, null)
            ?: return mutableListOf()
        return try {
            val arr = JSONArray(raw)
            val out = mutableListOf<Dev>()
            for (i in 0 until arr.length()) {
                val o = arr.getJSONObject(i)
                out.add(
                    Dev(
                        id = o.getString("id"), name = o.getString("name"), kind = o.getString("kind"),
                        scheme = o.optString("scheme", "http"), host = o.getString("host"),
                        port = o.getInt("port"), pairKey = o.optString("pairKey", ""),
                        url = o.optString("url", ""), addedAt = o.optLong("addedAt", 0L),
                        alias = o.optString("alias", ""),
                    )
                )
            }
            out
        } catch (e: Exception) {
            mutableListOf()
        }
    }

    fun save(ctx: Context, devs: List<Dev>) {
        val arr = JSONArray()
        devs.forEach { d ->
            arr.put(
                JSONObject()
                    .put("id", d.id).put("name", d.name).put("kind", d.kind)
                    .put("scheme", d.scheme).put("host", d.host).put("port", d.port)
                    .put("pairKey", d.pairKey).put("url", d.url).put("addedAt", d.addedAt)
                    .put("alias", d.alias)
            )
        }
        ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit()
            .putString(KEY_DEVS, arr.toString()).apply()
    }

    /** 返回 false = 同 id 已存在 */
    fun add(ctx: Context, dev: Dev): Boolean {
        val devs = load(ctx)
        if (devs.any { it.id == dev.id }) return false
        devs.add(dev)
        save(ctx, devs)
        return true
    }

    fun remove(ctx: Context, id: String) {
        val devs = load(ctx)
        devs.removeAll { it.id == id }
        save(ctx, devs)
        if (lastDevice(ctx) == id) setLastDevice(ctx, null)
    }

    fun byId(ctx: Context, id: String): Dev? = load(ctx).firstOrNull { it.id == id }

    /** 设置/清除备注（空串 = 清除，恢复默认显示名）。返回是否找到了这台设备 */
    fun setAlias(ctx: Context, id: String, alias: String): Boolean {
        val devs = load(ctx)
        val i = devs.indexOfFirst { it.id == id }
        if (i < 0) return false
        devs[i] = devs[i].copy(alias = alias.trim())
        save(ctx, devs)
        return true
    }

    fun lastDevice(ctx: Context): String? =
        ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getString(KEY_LAST, null)

    fun setLastDevice(ctx: Context, id: String?) {
        ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit()
            .putString(KEY_LAST, id).apply()
    }
}

/**
 * 缓存清理。三层：
 * - 单台电脑：清它的独立 Profile（站点数据 + 尽力删整域）；旧 WebView（<114 无多
 *   Profile）退化为「临时 WebView 打开该源后 localStorage.clear()」
 * - 删除电脑：同上（清干净再走）
 * - 整个应用：所有设备 Profile + 默认域（HTTP 缓存 + localStorage）+ 应用 cacheDir
 *
 * 注意：ProfileStore / WebView 系列调用都要求在主线程（否则抛
 * "Must be called on the UI thread"）——这里统一由 IO 线程调度到主线程执行并等结果。
 */
object Cache {

    /** 清一台电脑在本机的全部缓存；返回是否成功（阻塞，供 IO 线程调用） */
    fun clearDevice(ctx: Context, dev: Dev): Boolean {
        if (!WebViewFeature.isFeatureSupported(WebViewFeature.MULTI_PROFILE)) {
            return clearViaTempWebView(ctx, dev)
        }
        val name = dev.profileName()
        val main = Handler(Looper.getMainLooper())
        val res = booleanArrayOf(false) // 站点数据已清
        runOnMain(main) {
            try {
                val store = ProfileStore.getInstance()
                val profile = store.getProfile(name)
                if (profile == null) {
                    res[0] = true // 从未打开过：无数据
                } else {
                    try {
                        profile.webStorage.deleteAllData()
                        res[0] = true
                    } catch (e: Exception) {
                        android.util.Log.d("JxLink", "webStorage.clear: ${e.message}")
                    }
                }
            } catch (e: Exception) {
                android.util.Log.d("JxLink", "clearDevice(store): ${e.message}")
            }
        }
        // 尽力删整个 Profile（连同磁盘缓存）。Profile 在本进程里刚被 WebView 用过时
        // 可能删不动（销毁异步）——小退避重试；删不动不算失败：站点数据已清，
        // 磁盘缓存待进程重启后可删
        var tries = 0
        while (tries++ < 8) {
            val del = booleanArrayOf(false)
            runOnMain(main) {
                try {
                    del[0] = ProfileStore.getInstance().deleteProfile(name)
                    if (del[0]) android.util.Log.d("JxLink", "deleteProfile ok (try $tries)")
                } catch (e: Exception) {
                    android.util.Log.d("JxLink", "deleteProfile: ${e.message}")
                }
            }
            if (del[0]) return true
            try {
                Thread.sleep(300)
            } catch (e: InterruptedException) {
            }
        }
        android.util.Log.d("JxLink", "deleteProfile gave up, storageCleared=${res[0]}")
        return res[0]
    }

    /** 清整个应用的缓存（阻塞，供 IO 线程调用） */
    fun clearAll(ctx: Context): Boolean {
        val ok = booleanArrayOf(true)
        val main = Handler(Looper.getMainLooper())
        runOnMain(main) {
            var w: WebView? = null
            try {
                // 所有设备 Profile（删不动的——本进程刚用过——退化为清它的站点数据）
                if (WebViewFeature.isFeatureSupported(WebViewFeature.MULTI_PROFILE)) {
                    try {
                        val store = ProfileStore.getInstance()
                        for (n in store.allProfileNames) {
                            if (n == Profile.DEFAULT_PROFILE_NAME) continue
                            var done = false
                            try {
                                done = store.deleteProfile(n)
                            } catch (e: Exception) {
                            }
                            if (!done) {
                                try {
                                    store.getProfile(n)?.webStorage?.deleteAllData()
                                    done = true
                                } catch (e: Exception) {
                                    android.util.Log.d("JxLink", "clearAll($n): ${e.message}")
                                }
                            }
                            if (!done) ok[0] = false
                        }
                    } catch (e: Exception) {
                        android.util.Log.d("JxLink", "clearAll(profiles): ${e.message}")
                    }
                }
                // 默认域：临时 WebView 一次性清 HTTP 缓存 + 全部站点数据
                w = WebView(ctx)
                w!!.settings.javaScriptEnabled = true
                w!!.settings.domStorageEnabled = true
                w!!.clearCache(true)
                WebStorage.getInstance().deleteAllData()
                try {
                    android.webkit.CookieManager.getInstance().removeAllCookies(null)
                } catch (e: Exception) {
                }
            } catch (e: Exception) {
                android.util.Log.d("JxLink", "clearAll: ${e.message}")
                ok[0] = false
            } finally {
                try {
                    w?.destroy()
                } catch (e: Exception) {
                }
            }
        }
        // 应用自身缓存目录
        try {
            ctx.cacheDir.deleteRecursively()
        } catch (e: Exception) {
        }
        return ok[0]
    }

    /** 在主线程跑一段代码并等它结束（超时 8s） */
    private fun runOnMain(main: Handler, block: () -> Unit) {
        val latch = CountDownLatch(1)
        main.post {
            try {
                block()
            } finally {
                latch.countDown()
            }
        }
        try {
            latch.await(8, TimeUnit.SECONDS)
        } catch (e: InterruptedException) {
        }
    }

    /** 回退路径：临时 WebView 打开该电脑入口拿到源后清 localStorage（旧 WebView 无多 Profile 时用） */
    private fun clearViaTempWebView(ctx: Context, dev: Dev): Boolean {
        val latch = CountDownLatch(1)
        val ok = booleanArrayOf(false)
        val main = Handler(Looper.getMainLooper())
        val holder = arrayOfNulls<WebView>(1)
        main.post {
            try {
                val w = WebView(ctx)
                holder[0] = w
                w.settings.javaScriptEnabled = true
                w.settings.domStorageEnabled = true
                w.settings.cacheMode = WebSettings.LOAD_NO_CACHE
                w.webViewClient = object : WebViewClient() {
                    override fun onPageFinished(v: WebView, url: String?) {
                        v.evaluateJavascript("try{localStorage.clear();sessionStorage.clear()}catch(e){}") {
                            ok[0] = true
                            latch.countDown()
                        }
                    }

                    override fun onReceivedError(v: WebView, req: WebResourceRequest, err: WebResourceError) {
                        if (req.isForMainFrame) latch.countDown()
                    }
                }
                w.loadUrl(dev.entryUrl())
            } catch (e: Exception) {
                latch.countDown()
            }
        }
        try {
            latch.await(15, TimeUnit.SECONDS)
        } catch (e: InterruptedException) {
        }
        main.post {
            try {
                holder[0]?.stopLoading()
                holder[0]?.destroy()
            } catch (e: Exception) {
            }
        }
        return ok[0]
    }
}
