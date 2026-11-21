package com.jianxiang.link

import android.app.Activity
import android.app.AlertDialog
import android.content.Intent
import android.graphics.Color
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.view.Gravity
import android.view.View
import android.widget.Button
import android.widget.EditText
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.TextView
import android.widget.Toast
import java.util.concurrent.Executors
import kotlin.math.abs

/**
 * 首页：顶上「扫码连接 / 设置」两行按钮，下面设备列表。
 * - 每行一台电脑：在线/离线（离线整行灰字），点击进入
 * - 长按 → 清空缓存 / 删除（均二次确认）
 * - 冷启动按记忆直达上次所在的电脑页（退出时在哪台、下次就在哪台）
 */
class MainActivity : Activity() {

    private lateinit var devList: LinearLayout
    private lateinit var emptyHint: TextView
    private lateinit var labelDevices: TextView
    private lateinit var banner: LinearLayout
    private lateinit var bannerText: TextView
    private var devs: List<Dev> = emptyList()
    private val online = HashMap<String, Boolean>()   // id -> 在线态（内存，随探测刷新）
    private val main = Handler(Looper.getMainLooper())
    private val io = Executors.newCachedThreadPool()
    private var unlistenDownload: (() -> Unit)? = null

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)
        devList = findViewById(R.id.dev_list)
        emptyHint = findViewById(R.id.empty_hint)
        labelDevices = findViewById(R.id.label_devices)
        banner = findViewById(R.id.update_banner)
        bannerText = findViewById(R.id.update_banner_text)
        // 横幅点文字 = 打开下载/安装流程；点 ✕ = 这个版本不再提醒（出现更新的版本才再提示）
        banner.setOnClickListener {
            Update.lastFound(this)?.let { info -> Update.showFlow(this, info) }
        }
        findViewById<TextView>(R.id.update_banner_close).setOnClickListener {
            Update.lastFound(this)?.let { info -> Update.dismissBanner(this, info.version) }
            banner.visibility = View.GONE
        }
        // 下载状态变化（后台下载时横幅文字跟着走：进度 → 已就绪点击安装）
        unlistenDownload = UpdateDownload.listen { if (!isDestroyed) renderBanner() }
        findViewById<Button>(R.id.btn_scan).setOnClickListener {
            startActivityForResult(Intent(this, ScanActivity::class.java), REQ_SCAN)
        }
        findViewById<Button>(R.id.btn_settings).setOnClickListener {
            startActivity(Intent(this, SettingsActivity::class.java))
        }
        findViewById<Button>(R.id.btn_downloads).setOnClickListener {
            startActivity(Intent(this, DownloadsActivity::class.java))
        }

        // 记忆位置：上次退出时停留在某台电脑里 → 直接进那台。
        // 只在「全新启动」时做：进程被杀后系统恢复任务栈时 savedInstanceState
        // 非空、栈上本来就有设备页，再拉一个会叠出两层设备页
        val last = if (savedInstanceState == null) DeviceStore.lastDevice(this) else null
        if (last != null && DeviceStore.byId(this, last) != null) {
            startActivity(Intent(this, DeviceActivity::class.java).putExtra(DeviceActivity.EXTRA_ID, last))
        }
    }

    override fun onResume() {
        super.onResume()
        // 这里【不】清「上次所在」记忆：本页出现 ≠ 用户回了首页。系统杀掉进程后按
        // saved state 恢复任务栈时，栈底的本页也会走一遍 onCreate/onResume（设备页
        // 是随后才重建的），那时清记忆等于把刚恢复的位置抹掉——用户从外面重进 APP
        // 就再也进不到上次那台电脑（2026-10-22 实测）。清记忆的职责归触发点：
        // 用户主动回首页（设备页浮条/返回键/边缘手势 → DeviceActivity.finish()）。
        updateDownloadsButton()
        refresh()
        renderBanner()
        silentUpdateCheck()
    }

    /** 首页「下载管理」按钮带上数量（下完文件回首页一眼能看到有几个） */
    private fun updateDownloadsButton() {
        val n = try { DownloadStore.list(this).size } catch (e: Exception) { 0 }
        findViewById<Button>(R.id.btn_downloads).text =
            if (n > 0) "下载管理（$n）" else "下载管理"
    }

    /* ---------- 更新横幅与静默检查 ---------- */

    /** 静默检查（节流 1 小时）：失败一声不吭只记日志，成功且比当前新 → 记下清单并亮横幅 */
    private fun silentUpdateCheck() {
        if (!Update.checkDue(this)) return
        Update.markChecked(this)
        io.execute {
            try {
                Thread.sleep(1200) // 不与启动抢网络
                val r = Update.check(this)
                if (r is Update.Check.Err) {
                    android.util.Log.d("JxLink", "silent update check: ${r.msg}")
                }
            } catch (e: Exception) {
                android.util.Log.d("JxLink", "silent update check: ${e.message}")
            }
            main.post { if (!isDestroyed) renderBanner() }
        }
    }

    /** 横幅出现条件：查到过更新 + 仍比当前新 + 没被用户关掉；文字随下载状态走 */
    private fun renderBanner() {
        val found = Update.lastFound(this)
        if (found == null || Update.versionCompare(found.version, Update.currentVersion(this)) <= 0) {
            if (found != null) Update.clearLastFound(this)
            banner.visibility = View.GONE
            return
        }
        if (found.version == Update.bannerDismissed(this)) {
            banner.visibility = View.GONE
            return
        }
        banner.visibility = View.VISIBLE
        val dl = UpdateDownload.state
        bannerText.text = when {
            dl is UpdateDownload.State.Running && dl.info.url == found.url ->
                if (dl.total > 0) "正在下载新版本 ${found.version}… ${dl.got * 100 / dl.total}%"
                else "正在下载新版本 ${found.version}…"
            dl is UpdateDownload.State.Done && dl.info.url == found.url ->
                "新版本 ${found.version} 已就绪，点击安装"
            else -> "发现新版本 ${found.version}，点击更新"
        }
    }

    override fun onDestroy() {
        super.onDestroy()
        unlistenDownload?.invoke()
        io.shutdown()
    }

    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode == REQ_SCAN && resultCode == RESULT_OK) {
            val entry = data?.getStringExtra("entry") ?: return
            addEntry(entry)
        }
    }

    private fun addEntry(entry: String) {
        val dev = Relay.parseEntry(entry)
        if (dev == null) {
            Toast.makeText(this, "不是有效的连接二维码（应为电脑端 设置·网络访问 的入口码）", Toast.LENGTH_LONG).show()
            return
        }
        if (!DeviceStore.add(this, dev)) {
            Toast.makeText(this, "「${dev.name}」已经添加过了", Toast.LENGTH_SHORT).show()
            refresh()
            return
        }
        Toast.makeText(this, "已添加「${dev.name}」", Toast.LENGTH_SHORT).show()
        refresh()
    }

    /* ---------- 列表与探测 ---------- */

    private fun refresh() {
        devs = DeviceStore.load(this)
        render()
        for (d in devs) {
            io.execute {
                val on = Relay.ping(d)
                main.post {
                    online[d.id] = on
                    if (!isDestroyed) render()
                }
            }
        }
    }

    private fun render() {
        devList.removeAllViews()
        val empty = devs.isEmpty()
        emptyHint.visibility = if (empty) View.VISIBLE else View.GONE
        labelDevices.visibility = if (empty) View.GONE else View.VISIBLE
        for (d in devs) devList.addView(devRow(d))
    }

    private fun dp(v: Float): Int = (v * resources.displayMetrics.density + 0.5f).toInt()

    private fun devRow(d: Dev): View {
        val on = online[d.id]
        val ctx = this
        val row = LinearLayout(ctx).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = Gravity.CENTER_VERTICAL
            setBackgroundResource(R.drawable.bg_dev_row)
            setPadding(dp(14f), dp(12f), dp(12f), dp(12f))
            layoutParams = LinearLayout.LayoutParams(
                LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT,
            ).apply { bottomMargin = dp(10f) }
        }

        val icon = ImageView(ctx).apply {
            setImageResource(R.drawable.ic_monitor)
            if (on == false) setColorFilter(Color.parseColor("#9AA0A6"))
        }
        row.addView(
            icon, LinearLayout.LayoutParams(dp(24f), dp(24f)).apply { marginEnd = dp(12f) },
        )

        val mid = LinearLayout(ctx).apply { orientation = LinearLayout.VERTICAL }
        mid.addView(
            TextView(ctx).apply {
                text = d.displayName()   // 备注优先；无备注 = 设备默认名
                textSize = 15f
                setTypeface(typeface, android.graphics.Typeface.BOLD)
                setTextColor(if (on == false) Color.parseColor("#9AA0A6") else Color.parseColor("#1F2329"))
            },
        )
        mid.addView(
            TextView(ctx).apply {
                textSize = 12f
                // 状态前带上原名，方便对照（仅在设置了备注时）
                val state = when (on) {
                    null -> "检测中…"; true -> "在线"; false -> "离线"
                }
                text = if (d.alias.isNotBlank() && d.alias != d.name) "${d.name} · $state" else state
                setTextColor(
                    when (on) {
                        null -> Color.parseColor("#8A909C")
                        true -> Color.parseColor("#2EA043")
                        false -> Color.parseColor("#9AA0A6")
                    }
                )
            },
        )
        row.addView(
            mid, LinearLayout.LayoutParams(0, LinearLayout.LayoutParams.WRAP_CONTENT, 1f),
        )

        row.addView(
            ImageView(ctx).apply {
                setImageResource(R.drawable.ic_chevron)
                if (on == false) alpha = 0.5f
            },
        )

        row.setOnClickListener { openDevice(d.id) }
        row.setOnLongClickListener {
            longPressMenu(d)
            true
        }
        return row
    }

    private fun openDevice(id: String) {
        startActivity(Intent(this, DeviceActivity::class.java).putExtra(DeviceActivity.EXTRA_ID, id))
    }

    /* ---------- 长按：备注 / 清空缓存 / 删除（后两者二次确认） ---------- */

    private fun longPressMenu(d: Dev) {
        AlertDialog.Builder(this)
            .setTitle(d.displayName())
            .setItems(arrayOf("备注", "清空缓存", "删除")) { _, which ->
                when (which) {
                    0 -> editAlias(d)
                    1 -> confirmClearCache(d)
                    2 -> confirmDelete(d)
                }
            }
            .show()
    }

    /** 长按菜单·备注：给设备起个好认的名字（如「办公室电脑」），空 = 恢复默认显示 */
    private fun editAlias(d: Dev) {
        val input = EditText(this).apply {
            hint = "如：办公室电脑 / 车间那台"
            setText(d.alias)
            setSingleLine()
            val pad = dp(20f)
            setPadding(pad, dp(14f), pad, dp(6f))
        }
        AlertDialog.Builder(this)
            .setTitle("备注")
            .setMessage("给「${d.name}」起个好认的名字，方便区分多台设备。\n留空则恢复默认显示。")
            .setView(input)
            .setPositiveButton("保存") { _, _ ->
                val v = input.text.toString()
                DeviceStore.setAlias(this, d.id, v)
                Toast.makeText(
                    this,
                    if (v.isBlank()) "已清除备注，恢复默认显示" else "已保存备注「${v.trim()}」",
                    Toast.LENGTH_SHORT,
                ).show()
                refresh()
            }
            .setNegativeButton("取消", null)
            .show()
    }

    private fun confirmClearCache(d: Dev) {
        AlertDialog.Builder(this)
            .setTitle("清空缓存")
            .setMessage("确定清空「${d.displayName()}」在本机保存的全部缓存与页面数据？\n\n不影响电脑端数据。")
            .setPositiveButton("清空") { _, _ ->
                io.execute {
                    val ok = Cache.clearDevice(this, d)
                    main.post {
                        Toast.makeText(
                            this, if (ok) "已清空「${d.displayName()}」的缓存" else "清空未完全成功（页面可能仍开着，稍后再试）", Toast.LENGTH_SHORT,
                        ).show()
                    }
                }
            }
            .setNegativeButton("取消", null)
            .show()
    }

    private fun confirmDelete(d: Dev) {
        AlertDialog.Builder(this)
            .setTitle("删除")
            .setMessage("确定删除「${d.displayName()}」？\n\n删除后需要重新扫码才能再连接，其缓存也会一并清除。")
            .setPositiveButton("删除") { _, _ ->
                io.execute {
                    Cache.clearDevice(this, d)
                    DeviceStore.remove(this, d.id)
                    main.post {
                        online.remove(d.id)
                        Toast.makeText(this, "已删除「${d.displayName()}」", Toast.LENGTH_SHORT).show()
                        refresh()
                    }
                }
            }
            .setNegativeButton("取消", null)
            .show()
    }

    companion object {
        private const val REQ_SCAN = 1
    }
}
