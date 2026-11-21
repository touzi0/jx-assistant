package com.jianxiang.link

import android.app.Activity
import android.app.AlertDialog
import android.content.Intent
import android.net.Uri
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.provider.OpenableColumns
import android.webkit.MimeTypeMap
import android.widget.Toast
import androidx.core.content.IntentCompat
import java.io.File
import java.io.FileOutputStream
import java.util.concurrent.Executors

/**
 * 系统分享入口：微信（文件「用其他应用打开」、图片/视频「其他应用」）、文件管理器、
 * 浏览器等一切 ACTION_SEND / SEND_MULTIPLE / ACTION_VIEW 都落在这里。
 *
 * 流程：
 * 1. 先在授权存活期内把 content:// 内容复制进自己的缓存目录（微信授予的
 *    读取授权随本 Activity 生死，不能指望设备页再回来读它）；
 * 2. 单台电脑直送；多台弹选择（默认选中「上次所在」那台）；
 * 3. 带 EXTRA_SHARE_DIR / EXTRA_SHARE_TEXT 启动设备页——设备页已在栈顶时经
 *    onNewIntent 原地送达（不重载网页），否则等页面就绪后注入；
 * 4. 设备页把文件构造成 File 塞进输入框附件区（见 DeviceActivity.share 一节，
 *    网页侧与 SAF 选择完全同一条 multipart 通路）。
 *
 * 兜底：没有配对电脑 → 提示先扫码并回首页（本次分享内容只能请用户重发）；
 * 复制一半被系统杀 → 半截目录由下次进入本页时按「陈旧缓存」清掉。
 */
class ShareActivity : Activity() {

    private val main = Handler(Looper.getMainLooper())
    private val io = Executors.newSingleThreadExecutor()

    /** 收到的一个待转文件（URI + 元信息） */
    data class SharedRef(val uri: Uri, val name: String, val mime: String, val size: Long)

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        cleanStale()
        val (streams, text) = collect(intent)

        if (streams.isEmpty() && text.isNullOrBlank()) {
            Toast.makeText(this, "没有收到可分享的内容", Toast.LENGTH_SHORT).show()
            finish()
            return
        }

        val devs = DeviceStore.load(this)
        if (devs.isEmpty()) {
            Toast.makeText(this, "还没有连接电脑：先扫码连接一台，再回原应用重新分享", Toast.LENGTH_LONG).show()
            startActivity(
                Intent(this, MainActivity::class.java)
                    .addFlags(
                        Intent.FLAG_ACTIVITY_NEW_TASK or
                            Intent.FLAG_ACTIVITY_CLEAR_TOP or
                            Intent.FLAG_ACTIVITY_SINGLE_TOP,
                    ),
            )
            finish()
            return
        }

        if (devs.size == 1) {
            copyAndGo(devs[0], streams, text)
            return
        }
        // 多台电脑：选择送哪台（默认上次所在；取消了就丢弃本次分享）
        val last = DeviceStore.lastDevice(this)
        val checked = devs.indexOfFirst { it.id == last }.let { if (it >= 0) it else 0 }
        AlertDialog.Builder(this)
            .setTitle("分享送到哪台电脑？")
            .setSingleChoiceItems(devs.map { it.displayName() }.toTypedArray(), checked) { _, _ -> }
            .setPositiveButton("送入") { d, _ ->
                val lv = (d as? AlertDialog)?.listView?.checkedItemPosition ?: checked
                copyAndGo(devs[if (lv in devs.indices) lv else checked], streams, text)
            }
            .setNegativeButton("取消") { _, _ -> finish() }
            .setOnCancelListener { finish() }
            .show()
    }

    /* ---------- 收集：ClipData / EXTRA_STREAM（单个或列表）/ data（VIEW）/ EXTRA_TEXT ---------- */

    private fun collect(i: Intent?): Pair<List<SharedRef>, String?> {
        val uris = LinkedHashSet<String>() // 去重保序
        val add = { u: Uri? -> if (u != null && u.scheme != null) uris.add(u.toString()) }

        val cd = i?.clipData
        if (cd != null) for (n in 0 until cd.itemCount) add(cd.getItemAt(n).uri)

        IntentCompat.getParcelableExtra(i ?: Intent(), Intent.EXTRA_STREAM, Uri::class.java)?.let(add)
        IntentCompat.getParcelableArrayListExtra(i ?: Intent(), Intent.EXTRA_STREAM, Uri::class.java)?.forEach(add)

        if (i?.action == Intent.ACTION_VIEW) add(i.data) // 微信文件「用其他应用打开」

        val text = try {
            i?.takeIf { it.action == Intent.ACTION_SEND }?.getStringExtra(Intent.EXTRA_TEXT)
        } catch (e: Exception) { null }

        val refs = ArrayList<SharedRef>()
        for (s in uris) refs.add(meta(Uri.parse(s), i?.type))
        return refs to text
    }

    /** 名称/大小走 ContentResolver 查询，mime 三级回退（intent → resolver → 扩展名） */
    private fun meta(uri: Uri, intentMime: String?): SharedRef {
        var name: String? = null
        var size = -1L
        try {
            contentResolver.query(uri, null, null, null, null)?.use { c ->
                if (c.moveToFirst()) {
                    val ni = c.getColumnIndex(OpenableColumns.DISPLAY_NAME)
                    if (ni >= 0 && !c.isNull(ni)) name = c.getString(ni)
                    val si = c.getColumnIndex(OpenableColumns.SIZE)
                    if (si >= 0 && !c.isNull(si)) size = c.getLong(si)
                }
            }
        } catch (e: Exception) {
        }
        if (name.isNullOrBlank()) {
            name = uri.lastPathSegment?.substringAfterLast('/')?.takeIf { it.isNotBlank() } ?: "shared"
        }
        name = name!!.replace('/', '_').replace('\\', '_').trim().take(150)
        if (name.isBlank()) name = "shared"

        var mime = intentMime?.takeIf { it.isNotBlank() && !it.endsWith("/*") && it != "*/*" }
            ?: try { contentResolver.getType(uri) } catch (e: Exception) { null }
        if (mime.isNullOrBlank()) {
            val ext = name!!.substringAfterLast('.', "").lowercase()
            if (ext.isNotEmpty()) mime = MimeTypeMap.getSingleton().getMimeTypeFromExtension(ext)
        }
        if (mime.isNullOrBlank()) mime = "application/octet-stream"
        return SharedRef(uri, name!!, mime, size)
    }

    /* ---------- 复制（授权存活期内）→ 送入设备页 ---------- */

    private fun copyAndGo(dev: Dev, refs: List<SharedRef>, text: String?) {
        val dlg = AlertDialog.Builder(this)
            .setCancelable(false)
            .setMessage("正在接收分享的内容…")
            .show()
        io.execute {
            val dir = File(cacheDir, "share-" + System.currentTimeMillis())
            dir.mkdirs()
            val copied = ArrayList<SharedRef>()
            val skipBig = ArrayList<String>()
            val broken = ArrayList<String>()
            val used = HashSet<String>()
            for (r in refs) {
                if (r.size > MAX_FILE_BYTES) { skipBig.add(r.name); continue }
                // 同名去重：a.pdf、a(2).pdf …
                val dot = r.name.lastIndexOf('.')
                val base = if (dot > 0) r.name.substring(0, dot) else r.name
                val ext = if (dot > 0) r.name.substring(dot) else ""
                var out = r.name
                var n = 2
                while (!used.add(out)) { out = "$base($n)$ext"; n++ }
                val f = File(dir, out)
                try {
                    contentResolver.openInputStream(r.uri)?.use { ins ->
                        FileOutputStream(f).use { outs -> ins.copyTo(outs, 256 * 1024) }
                    } ?: throw IllegalStateException("openInputStream null")
                    copied.add(r.copy(uri = Uri.fromFile(f)))
                } catch (e: Exception) {
                    broken.add(r.name)
                    f.delete()
                }
            }
            if (copied.isEmpty() && text.isNullOrBlank()) {
                main.post {
                    if (isFinishing || isDestroyed) { dir.deleteRecursively(); return@post }
                    dlg.dismiss()
                    val why = when {
                        skipBig.isNotEmpty() -> "文件超过 ${MAX_FILE_BYTES / 1024 / 1024}MB，太大送不动"
                        else -> "读取分享内容失败（原应用可能已收回）"
                    }
                    Toast.makeText(this, why, Toast.LENGTH_LONG).show()
                    dir.deleteRecursively()
                    finish()
                }
                return@execute
            }
            // 送给设备页的是「复制好的目录」（纯文本分享没有目录）——文件名与目录内实际文件一致
            val names = copied.joinToString("、") { it.name }
            main.post {
                if (isFinishing || isDestroyed) { dir.deleteRecursively(); return@post }
                dlg.dismiss()
                if (skipBig.isNotEmpty() || broken.isNotEmpty()) {
                    val skipped = (skipBig.map { "「$it」过大" } + broken.map { "「$it」读取失败" }).joinToString("，")
                    Toast.makeText(this, "已跳过：$skipped", Toast.LENGTH_LONG).show()
                }
                android.util.Log.d(
                    "JxLink", "share -> dev=${dev.id} files=${copied.size} text=${!text.isNullOrBlank()} ($names)",
                )
                val i = Intent(this, DeviceActivity::class.java)
                    .putExtra(DeviceActivity.EXTRA_ID, dev.id)
                    .putExtra("share_names", names)
                if (copied.isNotEmpty()) i.putExtra(DeviceActivity.EXTRA_SHARE_DIR, dir.absolutePath)
                if (!text.isNullOrBlank()) i.putExtra(DeviceActivity.EXTRA_SHARE_TEXT, text)
                i.addFlags(
                    Intent.FLAG_ACTIVITY_NEW_TASK or
                        Intent.FLAG_ACTIVITY_CLEAR_TOP or
                        Intent.FLAG_ACTIVITY_SINGLE_TOP,
                )
                startActivity(i)
                finish()
            }
        }
    }

    /** 清掉历史遗留的半截分享目录（6 小时前的必是陈货；刚生成的最新一份不在此列） */
    private fun cleanStale() {
        val cutoff = System.currentTimeMillis() - 6 * 3600_000L
        cacheDir?.listFiles { d -> d.isDirectory && d.name.startsWith("share-") }?.forEach { d ->
            if (d.lastModified() < cutoff) d.deleteRecursively()
        }
    }

    override fun onDestroy() {
        super.onDestroy()
        io.shutdown()
    }

    companion object {
        private const val MAX_FILE_BYTES = 128L * 1024 * 1024 // 单文件上限（网页端 multipart 上限 256MB，手机内存取半）
    }
}
