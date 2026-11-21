package com.jianxiang.link

import android.app.Activity
import android.app.AlertDialog
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.view.View
import android.widget.Button
import android.widget.EditText
import android.widget.TextView
import android.widget.Toast
import java.util.concurrent.Executors

/**
 * 设置（单页四区域）：
 * 1. 检查更新 —— 当前/最新版本、手动检查；有更新时出现「下载并安装」（下载进度 →
 *    sha256 校验 → 系统安装器；Android 不能静默自装，见 Update.kt）
 * 2. 清空应用缓存 —— 所有电脑的独立 Profile + 默认域（HTTP 缓存/localStorage）+ 应用缓存
 * 3. 反馈意见 —— 标题+正文，封 multipart 帧经加密中继发 POST /api/feedback；
 *    依次尝试（上次所在电脑优先），第一台成功的为准
 * 4. 更新源 —— 清单地址可改（内测指向别的源），可恢复默认
 */
class SettingsActivity : Activity() {

    private val main = Handler(Looper.getMainLooper())
    private val io = Executors.newSingleThreadExecutor()

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_settings)

        findViewById<TextView>(R.id.upd_current).text = "当前版本：${Update.currentVersion(this)}"
        findViewById<TextView>(R.id.upd_status).text = "还没有检查过更新"
        findViewById<Button>(R.id.btn_check_update).setOnClickListener { checkUpdate() }
        findViewById<Button>(R.id.btn_download_update).setOnClickListener {
            Update.lastFound(this)?.let { Update.showFlow(this, it) }
        }
        findViewById<EditText>(R.id.upd_source).setText(Update.source(this))
        findViewById<Button>(R.id.btn_source_save).setOnClickListener { saveSource() }
        findViewById<Button>(R.id.btn_source_reset).setOnClickListener {
            Update.setSource(this, null)
            findViewById<EditText>(R.id.upd_source).setText(Update.DEFAULT_BASE)
            Toast.makeText(this, "已恢复默认更新源", Toast.LENGTH_SHORT).show()
        }

        findViewById<Button>(R.id.btn_clear_cache).setOnClickListener { confirmClear() }
        findViewById<Button>(R.id.btn_feedback).setOnClickListener { sendFeedback() }
    }

    override fun onResume() {
        super.onResume()
        // 从系统安装器回来可能已经装上了新版本，回来刷新一下口径
        findViewById<TextView>(R.id.upd_current).text = "当前版本：${Update.currentVersion(this)}"
    }

    override fun onDestroy() {
        super.onDestroy()
        io.shutdown()
    }

    /* ---------- 检查更新与更新源 ---------- */

    private fun checkUpdate() {
        val btn = findViewById<Button>(R.id.btn_check_update)
        val status = findViewById<TextView>(R.id.upd_status)
        val dl = findViewById<Button>(R.id.btn_download_update)
        btn.isEnabled = false
        btn.text = "检查中…"
        status.text = "正在检查更新…"
        dl.visibility = View.GONE
        io.execute {
            val r = Update.check(this)
            main.post {
                if (isDestroyed) return@post
                btn.isEnabled = true
                btn.text = "检查更新"
                when (r) {
                    is Update.Check.Ok -> if (r.newer) {
                        val i = r.info
                        status.text = buildString {
                            append("发现新版本 ").append(i.version)
                            if (i.publishedAt.isNotBlank()) append("（发布于 ").append(i.publishedAt.take(16)).append("）")
                            append("\n大小：").append(i.sizeText())
                            if (i.note.isNotBlank()) append("\n").append(i.note)
                        }
                        dl.visibility = View.VISIBLE
                    } else {
                        status.text = "已是最新版本（${r.info.version}）"
                    }
                    is Update.Check.Err -> status.text = "检查失败：${r.msg}"
                }
            }
        }
    }

    private fun saveSource() {
        val raw = findViewById<EditText>(R.id.upd_source).text.toString().trim()
        if (raw.isEmpty()) {
            Update.setSource(this, null)
            Toast.makeText(this, "已清空自定义，用默认更新源", Toast.LENGTH_SHORT).show()
            return
        }
        // 只认基地址：误贴的 /updates/... 路径与尾斜杠自动收敛掉
        val base = Update.normalizeBase(raw)
            ?: run {
                Toast.makeText(this, "地址要以 http:// 或 https:// 开头", Toast.LENGTH_SHORT).show()
                return
            }
        Update.setSource(this, base)
        findViewById<EditText>(R.id.upd_source).setText(base) // 回填收敛后的值
        Toast.makeText(
            this,
            if (base == raw) "已保存更新源" else "已保存：$base（后缀路径会自动补上）",
            Toast.LENGTH_LONG,
        ).show()
    }

    private fun confirmClear() {
        AlertDialog.Builder(this)
            .setTitle("清空应用缓存")
            .setMessage("确定清空所有电脑在本机的连接缓存与应用缓存？\n\n设备列表会保留；不影响电脑端数据。")
            .setPositiveButton("清空") { _, _ ->
                io.execute {
                    val ok = Cache.clearAll(this)
                    main.post {
                        Toast.makeText(
                            this,
                            if (ok) "已清空应用缓存" else "部分缓存未能清除（有页面可能仍开着）",
                            Toast.LENGTH_SHORT,
                        ).show()
                    }
                }
            }
            .setNegativeButton("取消", null)
            .show()
    }

    private fun sendFeedback() {
        val title = findViewById<EditText>(R.id.fb_title).text.toString().trim()
        val content = findViewById<EditText>(R.id.fb_content).text.toString().trim()
        if (title.isEmpty()) {
            Toast.makeText(this, "请填写标题", Toast.LENGTH_SHORT).show()
            return
        }
        if (content.isEmpty()) {
            Toast.makeText(this, "请填写正文", Toast.LENGTH_SHORT).show()
            return
        }
        val btn = findViewById<Button>(R.id.btn_feedback)
        btn.isEnabled = false
        btn.text = "发送中…"
        val ver = try {
            packageManager.getPackageInfo(packageName, 0).versionName ?: "?"
        } catch (e: Exception) {
            "?"
        }
        io.execute {
            // 上次所在的电脑优先，其余依次；第一台成功即止
            val last = DeviceStore.lastDevice(this)
            val devs = DeviceStore.load(this).sortedByDescending { it.id == last }
            var doneOn: Dev? = null
            var err = "没有可用的电脑——先扫码连接一台并保持其在线"
            for (d in devs) {
                when (val r = Relay.sendFeedback(d, title, content, ver)) {
                    is Relay.SendResult.Ok -> {
                        doneOn = d
                        break
                    }
                    is Relay.SendResult.Err -> err = r.msg
                }
            }
            val doneDev = doneOn
            main.post {
                if (isDestroyed) return@post
                btn.isEnabled = true
                btn.text = "发送反馈"
                if (doneDev != null) {
                    findViewById<EditText>(R.id.fb_title).setText("")
                    findViewById<EditText>(R.id.fb_content).setText("")
                    Toast.makeText(this, "已发送 ✓（经「${doneDev.name}」）", Toast.LENGTH_SHORT).show()
                } else {
                    Toast.makeText(this, "发送失败：$err", Toast.LENGTH_LONG).show()
                }
            }
        }
    }
}
