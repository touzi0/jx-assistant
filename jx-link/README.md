# 助理连接（jx-link）

坚祥数字助理的**独立轻量手机壳**：只做一件事——扫码连接电脑端的数字助理远程应用。
不带引擎、不带运行时（对比完整手机端约 103MB，本壳约 5MB），界面就是系统 WebView。

与完整手机端（`../软著版数字助理/android/`，包名 `com.jianxiang.assistant`）完全独立，
可同时安装；本壳包名 `com.jianxiang.link`，图标与现有应用同款 logo。

## 功能

- **扫码连接多台电脑**：扫电脑端 设置 · 网络访问 的入口二维码
  （`http://网关:端口/a/设备号#pk=配对密钥`），也支持局域网直连地址（`http://ip:端口`）。
  首页列表显示每台电脑的在线/离线（离线灰字），点击进入。
- **系统分享送入输入框（2026-10-19 v0.1.6）**：微信（文件「用其他应用打开」、
  图片/视频「其他应用」）、文件管理器、浏览器等任何 `ACTION_SEND` /
  `SEND_MULTIPLE` / `ACTION_VIEW` 都能发给本壳。流程：透明 `ShareActivity`
  在授权存活期内把 content:// 复制进私有缓存（应用零存储权限）→ 单台电脑直送、
  多台弹选择框（默认上次所在）→ 设备页等远程外壳把 `stageFiles` 准备好后，
  文件按 base64 分块注入，落进**输入框附件区**（与 SAF 选择完全同一条 multipart
  通路——跨会话切换保留、最终发送时才上送）；文字分享追加进主输入框。
  已在设备页里时经 `onNewIntent` 原地送达（模拟器实测 83ms，不重载网页）；
  单文件上限 128MB（网页端 multipart 上限 256MB，手机内存取半），超大文件
  跳过并提示。电脑端旧版本无 `JX.receiveFiles` 时自动回退全局 `stageFiles`。
- **电脑页**：WebView 打开入口，gate.html 自动经加密中继拉起应用外壳，与浏览器里
  的远程体验一致。顶部浮一条**应用级「⌂ 设备名 ⟳」胶囊**（原生控件浮在网页之上、
  屏幕水平绝对居中，像浏览器菜单栏；不参与网页布局，网页顶栏怎么排都不受影响——
  2026-10-12 改造，此前注入网页顶栏的方案在编程模式下会把右侧栏按钮挤到第二行），
  三格：左＝回首页、中＝设备名（过长省略）、右＝**刷新（2026-11-15 新增）**——
  点一下＝就地重连 + 补拉（重建 `/api/events` 事件流、abort 并重接当前会话执行流、
  `reloadMessages` 补回断线期间漏掉的消息），**不整页重载**（打到一半的输入与阅读
  位置都保住）；转圈时按钮禁用防连点，成功后浮条上显示 1 秒「已刷新」，失败提示
  「网络未恢复，请稍后再试」；**长按该按钮＝整页重载**兜底。
  注意：重连逻辑在电脑端前端（`web/app-chat.js` 的 `reconnectRemote`），电脑端
  未更新时按钮会直接按失败处理并提示走整页重载。从屏幕**顶缘下滑 / 底缘上滑**
  也回首页（仅认从边缘起手的滑动，中间滑对话不受影响，参数对齐电脑端左右栏手势）。
- **浏览器配套（2026-10-13 补齐）**：权限清单对齐浏览器类应用——
  RECORD_AUDIO + MODIFY_AUDIO_SETTINGS（缺后者报 Could not start audio source）、
  ACCESS_NETWORK_STATE（navigator.onLine 等网络感知）、POST_NOTIFICATIONS
  （Android 13+，下载完成通知；拒绝仅少通知）。文件上传走系统 SAF 选择器
  （content:// 授权回传，**应用自身零存储权限**）；文件下载挂 DownloadManager
  落公共 Downloads（WebView 默认点下载链接无响应，必须实现 DownloadListener）；
  网页全屏视频实现 onShowCustomView/onHideCustomView（默认也是无响应）。
  上传与麦克风已在模拟器（带音频）实测：picker 选图回传 File（名/大小/MIME 全对）、
  getUserMedia OK。
- **HTTPS 优先（2026-10-12）**：语音输入需要安全上下文（getUserMedia 仅 HTTPS）。
  壳子把 http 入口自动升级为 https 同端口加载，配对网关的自签证书自动「继续访问」
  （白名单只放行已配对 host，陌生站点照旧拒绝）；网关不支持 https 时自动回落
  http，一切如旧——浏览器扫码访问仍是 http，只有壳子享受 https。麦克风经
  `onPermissionRequest` 对配对 host 放行（首次弹系统权限框）。
  **麦克风权限清单是两个**：RECORD_AUDIO（运行时）+ **MODIFY_AUDIO_SETTINGS**
  （普通权限）——缺后者时 getUserMedia 报 `NotReadableError: Could not start
  audio source`（真机与模拟器都复现，2026-10-13 修复）。origin 匹配兼容
  `blob:https://host:port/...`（P2P 直连模式的远程外壳挂在 blob origin 上）。
- **返回键（2026-10-22 收敛）**：逻辑全在网页（`web/app-left.js` 的 `jxNavBack`）——
  设置页 → 3 秒内连按两次退出 → 弹出菜单 → 右栏 → 左栏 → 对话页呼出左栏，
  与浏览器后退、左右滑手势是同一套。壳只把按键转成一次 `jxNavBack()` 调用，只在
  网页回 `'exit'` 时 `finish()`（结束本页是网页做不到的事），另加「先退系统全屏
  视频」。**3 秒内第二次返回 = 回壳首页**（窗口也搬进了网页，与 App 内一致）。
- **打开页面 = 壳内浏览器页（2026-10-23）**：右栏浏览器与对话链接打开的页面开在
  壳自己的一层 `BrowserActivity`（工具栏：关闭 / 后退 / 前进 / 刷新 + 标题；
  2026-10-28 加**真地址栏**——独占一行，点一下聚焦即全选、长按菜单复制到的是完整
  网址（省略号只是显示截断）、回车跳转输入的地址，裸域名自动补 https://）。
  此前网页侧只能 `window.open(url,'_blank')`，而壳里没有接管新窗口
  （没实现 `WebChromeClient.onCreateWindow`）——请求要么被 WebView 静默丢掉，
  要么把电脑页 UI 顶掉：那页上没有 `jxNavBack`，返回键什么都不做，用户只能杀掉
  整个 App。现在**返回键先退本页历史、退不动就 `finish()` 回对话页**，右缘左滑
  同样关闭；这一层的关闭只在壳里收尾，**不调** `jxNavBack`（那是电脑页的层级）。
  工作区文件预览先在这一页里取成 base64 交壳落成缓存文件再 `file://` 打开
  （5MB 上限，超了直说去电脑上看）；网址与内容都由 `window.JXLink` 桥（
  `DeviceActivity` 注入）交给壳，没有桥的纯浏览器里仍是新标签页。
  代价：这一层用默认 WebView 域、不共享每台电脑的独立 Profile，站点登录态要重新登一次。
- **回桌面再回来（2026-10-22 定稿）**：**页面现场优先保留**——手机壳就该像一个
  浏览器，从桌面切回来还在原来那一页。
  **热路径**（进程活着）：点桌面图标只把任务栈前移到原处，WebView 不重建、不重载；
  **Activity 被系统回收但进程活着**：同样交给系统默认的 saved state 恢复，壳不插手
  （本类**不覆写** `onSaveInstanceState`）；**进程真被杀**：现场无从恢复，才重新走
  入口/中转页完整初始化，并由「上次所在电脑」+ 网页里的「上次会话」回到原处。
  首页 `launchMode` 用标准（图标重启不清掉栈上的设备页）+ `alwaysRetainTaskState`
  兜长后台；冷启动直达加 `savedInstanceState == null` 守卫（系统恢复任务栈时不重复
  叠设备页）。
  历史：2026-10-20 曾用「连 super 都不调的空 `onSaveInstanceState`」强迫冷路径整页
  重载，用来绕开「组件初始化不全」——那是拿正确机制补错误的洞，用户已否决，已删除。
- **组件（语音/待办）在切回来后一定在**：组件不是页面的一部分，而是 `web/cx.js`
  在文档就绪后经隧道（`/api/components`、`/api/components/<id>/client.js`，与主体
  数据同一条 fetch 劫持/中继通道）抓下来注入。页面被系统重建的那一瞬间隧道可能还
  没恢复，那一次抓不到组件就整页缺席、主体却正常——所以组件的重放挂在页面生命周期
  上（回到前台 / `pageshow` / 网络恢复 / 中继隧道就绪的 `jx:tunnel` 事件 / 30s 兜底），
  只重放「自己没起来过」的组件；用户主动关掉的组件绝不被复活（热开关语义不变）。
- **回到原对话（2026-10-22）**：「上次聊到哪」这条记忆由前端自己存
  （`localStorage['jx.lastSess']`，见
  `web/app.js` 的 `restoreLastSession`）——页面真被重载（进程被杀冷启动、外壳版本
  自愈整页重拉）时前端是从零启动的，靠它在会话列表到位后回到上次那个对话；热路径
  页面没重载，自然什么都不做、不跳首页。点「新工作任务」明确回首页时清掉这条记忆。
- **记忆位置**：退出应用时停在哪台电脑里，下次冷启动直接进那台；回到首页后再退出
  则下次从首页开始。清记忆只发生在**用户主动回首页**那一刻（`DeviceActivity.finish()`：
  首页浮条 / 双击返回 / 边缘手势）——2026-10-20 把它放在首页 `onResume` 里，系统恢复
  任务栈时首页也会走一遍 onResume，把刚恢复的位置抹掉，「进不到上次那台电脑」。
- **缓存管理**：
  - 长按设备行 → 清空缓存 / 删除（均二次确认；删除同时清缓存）
  - 设置 → 清空应用缓存（所有电脑 + 应用自身）
  - 每台电脑一个独立 WebView Profile（WebView 114+），缓存与 localStorage 互不串门。
    清空时优先 `ProfileStore.deleteProfile` 整域删除；Chromium 对**本进程内用过的**
    Profile 会拒绝删除（"Cannot delete in-use profile"，销毁异步且常驻），此时退化为
    `Profile.getWebStorage().deleteAllData()` 清站点数据（localStorage/外壳缓存——
    远程模式下有意义的数据都在这里），磁盘缓存待进程重启后删除
  - 旧 WebView（<114 无多 Profile）退化为共享域 + 临时 WebView 打开该源后
    `localStorage.clear()`
- **在线探测**：对网关 `POST /relay` 发一个几十字节的加密探测帧（GET /api/shell-hash），
  网关对不在线设备回 503——不上注册号（同号重复注册会互踢），只借中继通道探活。
- **下载管理（2026-10-27）**：电脑端交付卡的「下载」在网页里是 `fetch → blob →
  a[download]`（远程模式经隧道），浏览器能存，**WebView 存不了**——`DownloadListener`
  拿到 `blob:` 地址取不到内容，用户看到的直接是「下载失败」。现在壳里改成走桥：
  网页调 `JXLink.beginDownload/appendDownload/finishDownload`（512KB 一块 base64），
  壳落到私有目录 `files/downloads/`，首页第三个按钮「下载管理（N）」里可打开 / 分享 /
  删除 / 清空（打开走 FileProvider，不需要任何存储权限）。网页侧判定见
  `app-chat.js` 的 `jxShellBridge()`/`jxShellSaveURL()` 与 `remote-boot.js` 的
  `downloadURL`（桥可能在顶层文档，也可能在远程外壳的父窗口）。
  **下载并打开（2026-10-30）**：新增 `JXLink.openDownload(id)` 桥——交付卡「打开」
  在壳里下载完成后由网页紧接着调用，壳按 id 找到刚落盘的项走 FileProvider +
  ACTION_VIEW 调系统查看器打开（与「下载管理」列表里的手动打开同一实现
  `DownloadsActivity.openItem`；存储 mime 为空或是 octet-stream 时按扩展名查
  MimeTypeMap 兜底，否则 docx/xlsx 会被「无应用可打开」挡住）。旧壳没有这个
  桥 → 网页检测不到就只保存不打开，两侧互相兼容。
  **版本号口径（2026-10-27 改）**：与电脑端统一用 `YYYYMMDD-N`（同日第 N 版），
  `versionCode` 用 `YYYYMMDDNN`（如 2026102701），不再用 0.1.x 那套。
- **检查更新（2026-11-19）**：Android 不允许应用静默自装，所以只做标准路径——
  **查清单 → 下载 APK（进度/可后台/可取消）→ sha256 校验 → 交给系统安装器**
  （Android 8+ 首次安装需在系统设置授予「允许安装未知应用」，App 内引导）。
  - 更新源：内置默认基地址 `https://jxaizz.com:2227`（总端平台化地址，服务端接口
    约定见下），**设置里可改**（内测指向别的源）。只填基地址（要带 https:// 前缀），
    清单路径 `/updates/android/latest` 自动补上，误贴了路径也会自动收敛——与电脑端
    「公司服务器地址」口径一致；
  - 清单 JSON：`{"platform":"android","version":"2026xxxx-x","url":"<APK直链>","size":<字节>,"sha256":"<哈希>","note":"<说明>","publishedAt":"<时间>"}`，
    解析收在 `Update.kt` 的 `parseManifest`（服务端口径变了只改这一处）；
  - 设置页「检查更新」：当前版本、检查中/已是最新/发现新版本（含发布时间、大小、
    说明）与失败原因，一键进入下载安装流程；
  - 主页横幅：静默检查（1 小时节流）查到新版本时顶部出现「发现新版本 x，点击更新」，
    下载中显示进度、完成变「已就绪，点击安装」；✕ 关闭后**同一版本不再打扰**
    （记在本地，出现更新的版本才再提示）；
  - 证书策略与电脑端 `tlspolicy.go` 同口径：域名与内网 IP 放宽（自签源可连），
    公网 IP 严格校验；
  - 不打扰原则：网络不通/服务端 404 → 一切照常，只有手动「检查更新」才给一句失败。
- **设备备注名（2026-11-21）**：长按设备菜单第一项「备注」——给设备起个好认的名字
  （如「办公室电脑」「车间那台」）。有备注时列表主标题显示备注、副行带「原名 · 状态」；
  长按菜单标题、清缓存/删除确认、设备页名称 chip、分享时的设备选择列表均显示备注名；
  留空保存 = 清除备注恢复默认。备注随设备存 `jxlink` SharedPreferences（JSON `alias` 字段）。
- **设置页**：检查更新 + 更新源 + 清空应用缓存 + 反馈意见（multipart 封帧经中继发
  电脑端 `/api/feedback`，依次尝试、上次所在电脑优先）。

## 构建

```sh
sh build.sh        # 图标 → 签名 → gradlew assembleDebug
adb install -r app/build/outputs/apk/debug/app-debug.apk
```

依赖：JDK 17、Android SDK（compileSdk 35）、带 Pillow 的 Python（生成图标）。
工具链版本与主仓库安卓壳对齐（AGP 8.7.3 / Kotlin 2.1.0 / Gradle 8.9），
`android.overridePathCheck=true` 应对中文路径。

## 结构

```
app/src/main/java/com/jianxiang/link/
├── MainActivity.kt      首页：两行按钮（扫码连接/设置）+ 设备列表 + 长按菜单 + 二次确认
├── ScanActivity.kt      CameraX + ZXing 扫码（四向旋转解码，屏幕码横放也能认）
├── ShareActivity.kt     系统分享入口（SEND/SEND_MULTIPLE/VIEW）：复制到私有缓存 → 选电脑 → 送设备页
├── DeviceActivity.kt    WebView 电脑页：独立 Profile、应用级首页浮条、边缘手势、失败浮层、分享注入
├── BrowserActivity.kt   壳内浏览器页：手机端「打开页面」的落点（工具栏 + 返回键/右缘左滑关闭）
├── DeviceStore.kt       设备列表与「上次所在」记忆（SharedPreferences JSON）；Cache 清理
├── Relay.kt             中继协议客户端：入口解析、AES-GCM 封包、探活、反馈发送
├── Update.kt            检查更新：清单解析（parseManifest）、版本比较（同电脑端口径）、
│                        证书策略（域名/内网放宽）、APK 下载状态机（进度/取消/sha256 校验）、
│                        安装引导（FileProvider + 系统安装器 + 未知来源授权）与流程对话框
└── SettingsActivity.kt  设置页：检查更新 + 更新源 + 清空应用缓存 + 反馈意见
```

## 已在模拟器（AVD jx-test，Android 15 / WebView 124）点验

系统分享送入（2026-10-19，v0.1.6，宿主机 8443 挂模拟远程外壳页按真实契约暴露
stageFiles/#input 做端到端）：微信三通道对应的 intent 全走通——`ACTION_SEND`
文件（名/大小/MIME 全对入列）、`ACTION_SEND` 纯文本（落进输入框、input 事件
触发）、`ACTION_VIEW`（微信「用其他应用打开」同款）；多台电脑弹选择框（默认
上次所在）、「送入/取消」；已在设备页时 `onNewIntent` 原地送达不重载（实测
83ms）；连续两次分享（文字+文件）同页共存；顺带修了 LAN 直连入口在 WebView 124
上 https 回落失效的既有 bug（Chromium 给无路径 URL 补尾斜杠导致全等比对漏判，
改按 host+port+路径判）。真实微信 content:// 与 SEND_MULTIPLE 走同一收集/复制
代码路径，未单独实测。

回桌面再回来（2026-10-20 初测，2026-10-22 定稿，页面显示加载时间戳 + 服务端访问
日志双证）：修复前点图标必回首页（singleTask 清栈，已复现）；修复后——热路径
回桌面→图标仍停在原网页（加载时间不变、只有一个 onCreate）；进程被杀→图标 页面
重新导航到入口（加载时间刷新、服务器有真实文档 GET），且任务栈 Main+Device 各一个
不重复。2026-10-20 的初测还发现「默认行为下冷启动无文档 GET」（saved state 直出
旧页），当时按「宁走中转页」把状态还原禁掉了；2026-10-22 定为**现场优先保留**：
saved state 交还系统（不覆写 `onSaveInstanceState`），组件缺席改由页面侧自愈
（`web/cx.js` 的失败重放）解决，不再用整页重载回避。

首页两行按钮与设备列表（在线/离线灰字）、点击进电脑页（经真实 210 网关中继拉起
完整远程外壳）、应用级「⌂ 设备名」浮条（水平绝对居中、网页顶栏一行不被挤）、
HTTPS 升级 + 自签白名单（本机自签 https 服务器实测：http 入口自动升 https 加载、
无证书告警；网关仅 http 时自动回落）、麦克风权限链（getUserMedia → 系统权限框 →
grant 回传；模拟器无麦克风硬件止于 NotReadableError，真机即通）、返回键（对话页
呼出左栏 / 左栏收回对话页 / 3 秒双击回首页）、顶缘下滑/底缘上滑回首页（中部滑动不误触）、长按菜单与两处二次确认、清空缓存
（storage 清空 + deleteProfile 尽力）、删除设备、设置页两区域、反馈意见（真实发送
经中继落库电脑端 feedback 表，diagnostics 带 src=jx-link）、扫码页相机权限流程、
冷启动记忆直达（退出时在哪台电脑里，下次直接进哪台）。

**返回键改版（2026-10-22）待真机复验**：网页与 App 现在是同一套层级——
对话页返回→呼出左栏、左栏返回→回对话页、右栏返回→关右栏、3 秒内连按两次→
退出（浏览器离开网页 / App 回壳首页）。层级与双击窗口都在 `web/app-left.js` 的
`jxNavBack` 里；桩测覆盖见 `.jxai/tmp/nav/`（不随仓库发布）。

## 设计要点

- **注入按钮为何能活过换页**：gate.html 用 `document.open()/write()` 把配对页整页换成
  应用外壳，window 上的 JS 全局状态是存活的（gate.html 自己的顶层 const 残留冲突即证），
  守护 `setInterval` 装一次即可，页面内部重建由轮询幂等补挂。
- **明文 http**：网关入口都是 http（远程接入形态如此），manifest 开了
  `usesCleartextTraffic`；链路本身有端到端 AES-256-GCM（密钥=SHA-256(配对密钥)）。
- **不碰注册通道**：探活绝不向 `/hub` 注册同设备号（网关「同号新顶旧」会把电脑踢下线）。
