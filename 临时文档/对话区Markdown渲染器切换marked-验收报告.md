# 对话区 Markdown 渲染器切换验收报告（自研正则 → marked v12）

> 2026-11-15 · 改动范围：前端渲染与样式 + md-viewer 单一副本改造，后端逻辑零改动、未发布、未动服务器。
> 基线提交：`c74f417193`（改动全部未提交前的工作区状态，见文末 git 清单）。

## 1. 做了什么

| 文件 | 改动 |
|---|---|
| `web/marked.lib.js` | **新增**：marked v12.0.0 浏览器压缩版（MIT），从 `viewer-template.legacy.html` 逐字节抽取 + 头注释。全仓唯一一份 marked |
| `web/app-core.js` | `md()` 对外契约不变，实现换成 `marked.lexer`（GFM+breaks）+ 自研白名单渲染器（`renderInline`/`renderBlocks`/`renderLink`/`renderImage`/`mdPre`）；删掉旧 `inlineFmt`/旧 `renderBlocks`；`FILE_PATH_RE`/`codeSpanIsPath`/`pathLinks`/`esc` 原样保留 |
| `web/style.css` | `.md` 系列新增：引用块、分隔线、任务列表勾选框、嵌套列表外距、图片 max-width。字号全部继承 `--chat-fs`（em/相对值），未新增任何 px 字号 |
| `web/index.html` | 挂 `<script src="/marked.lib.js">`（app-core 之前） |
| `md-viewer/viewer-template.html` | 内联 marked 块（35,144 B）换成 `/*__MARKED__*/` 占位符，其余零改动 |
| `mdview.go` | 启动时从 `webFS`（go:embed web 的同一份字节）读 `marked.lib.js` 注入模板占位符；注入源异常退回原文（查看器显示「解析失败」，主功能不受影响） |
| `md-viewer/build.ps1` | 独立 MD查看器.exe 构建时注入同一份 marked（产物仍自包含单文件） |
| `md-viewer/tools/verify-toc.js` | 测试页构建同样注入 |
| 删除 | `viewer-template.src.html`、`viewer-template.legacy.html`、`tools/assemble-template.js`、`build_template.py`（组装层取消：模板即源文件；git 历史留档） |
| `mdview_test.go` | 新增 `TestMDViewerTemplateMarkedInjected`（注入链断了第一时间红） |
| `gui-test/md-render-test.js` | **新增**：83 例（旧 linkify 回归 + GFM 新语法 + XSS 白名单 + 视觉契约 + 用户消息胶囊红线） |
| `gui-test/linkify-test.js` | 改走新渲染器全链路（用例清单原样保留，16 例含 XSS） |
| `gui-test/md-perf-test.js` | **新增**：新旧性能对比（旧实现冻结快照内嵌，可长期重跑） |
| `THIRD-PARTY-NOTICES.md` §6、`AGENT.md`、`md-viewer/使用说明.txt` | 声明与文档同步 |

## 2. 语法拉齐（GFM 一次到位）

新增支持：**斜体**（`*x*`/`_x_`，intraword 下划线不误伤 `foo_bar_baz`）、**删除线**、**引用块**（可嵌套）、**分隔线**（`---`/`***`/`___`）、**任务列表**（勾选框）、**表格三向对齐**（`:---`/`:---:`/`---:`）、**远程图片**（懒加载+不带 referrer）、**嵌套列表**、**裸 URL 自动链接**（含 `www.`）、**缩进代码块**、**反斜杠转义**、**表格竖线转义 `\|`**、**有序列表 start**。
保留不动：文件路径 file-link（正文+反引号两路）、ext-link 点击行为、标题 h2 起步 h5 封顶、段落单换行=br、中文顿号有序列表（`1、` 预处理还原，列表标记不进渲染结果、无可见差异）。

## 3. 安全（自建，无第三方净化库）

**结论：不引入 DOMPurify。** 理由：md() 输出的每个标签都由自研渲染器白名单拼出（24 个标签：p/h2-h5/strong/em/del/br/a/code/pre/blockquote/hr/table/thead/tbody/tr/th/td/ul/ol/li/input/img），token 流里不存在「原样放行 HTML」的路径——html token 一律 esc 成文字（与旧渲染器行为一致），链接/图片只放行 `^https?://`（其余协议按原文转义显示，含 data:/javascript:/ftp:/mailto:），属性只由 esc 后的 href/src/alt/data-path 构成。DOMPurify（~22KB min）是「先放出 HTML 再过滤」的模型，对这套「只产出白名单」的渲染器没有增益，纯增体积与一个攻击面依赖。

XSS 实测（`gui-test/md-render-test.js`，14 组样例断言 + 真实浏览器 DOM 审计）：

| 样例 | 结果 |
|---|---|
| `<script>alert(1)</script>`（正文/代码块内/行内） | 全部 esc 成文字，DOM 里 0 个 script 标签 |
| `<img src=x onerror=alert(1)>` | esc 成文字，DOM 里 0 个带 onerror 的元素 |
| `[x](javascript:alert(1))` | 按原文转义显示 `[x](javascript:alert(1))`（与旧渲染器一致） |
| `<iframe>` / `<style>` | esc 成文字，0 个 iframe/style 标签 |
| `![x](data:text/html;base64,…)` / `![y](javascript:…)` | 图片协议白名单挡住，按原文转义 |
| `![z](https://a.b/c"onerror=alert(1))` | 引号被 esc 进 href 属性值内部，不逃出属性 |
| 输出全部 href/src | 全部匹配 `^https?://` |

浏览器 DOM 审计（对照页实测）：`new-out.querySelectorAll('.md *')` 的标签集合 = 白名单本身，无一多余。

## 4. 用户消息零影响（红线）

- `git diff` 证明 `web/app-chat.js` **零改动**：用户消息路径（`appendQuotedBody` → `appendQuoteBodyText`，「引用N」/文件名灰胶囊、mdBody=false 分支）一个字节没动。
- `gui-test/md-render-test.js` 用例 5 从 `app-chat.js` 现场抽取胶囊正则断言仍在原位、`“引用1”`/`“报告final.docx”` 两处命中胶囊形状。
- 对照页两侧各放了一个静态用户气泡示意（含 2 个灰胶囊），确认胶囊样式随主题/字号正常。

## 5. 视觉对照（用户重点）

**方法**：`​.jxai/tmp/md-compare/compare.html` 并排对照页——左列=旧渲染器（git HEAD 冻结副本），右列=新 marked 渲染器（从当前 app-core.js 现场抽取），同一份样例（`sample.md`，覆盖任务书列的全部 14 类场景），套真实 `web/style.css`，支持亮/暗主题切换与 `--chat-fs` 字号滑杆（12–22px）。已用独立浏览器窗口实测。

**计算样式逐项数值对照**（亮色 14px 与暗色 18px 各测一轮，全部 same=true）：

| 项目 | 旧 | 新 | 一致 |
|---|---|---|---|
| 段落 p 字号/上下边距/行高 | 14px / 8px / 24.5px | 同 | ✅ |
| h2 / h3 / h4 字号 | 16.94 / … / 14px | 同 | ✅ |
| 行内 code 字体/底色/内边距/圆角 | Consolas / 242,243,245 / 1.5px 6px / 5px | 同 | ✅ |
| 代码块底色/内边距/圆角/行高 | 246,248,250 / 12px 14px / 10px / 21.7px | 同 | ✅ |
| th/td 内边距/边框/底色 | 6px 10px / 1px / 同 | 同 | ✅ |
| 表格 display/边框合并/字号 | block / collapse / 同 | 同 | ✅ |
| ext-link 颜色/下划线样式 | 77,107,254 / 虚线 | 同 | ✅ |
| li 外距 / ul 缩进 | 3px / 24px | 同 | ✅ |
| 暗色全套颜色（正文/代码底/表格边框/链接） | — | — | ✅ 全同 |
| 字号联动（--chat-fs 18px） | p=18px, h2=21.78px, code=18px | 同 | ✅ |

**新语法样式**（旧渲染器没有，新增项）：引用块=3px 左边线（--border-2）+ 次级文字色 + 12px 左内边距；分隔线=1px --border；勾选框=accent-color 主题色、字号跟随 em；图片 max-width:100%。均不引入新字号体系。

**文件路径/链接识别对照**（整份样例）：file-link 7 个 = 旧 7 个；ext-link 7 个 = 旧 7 个（识别面零回归）。

截图存档（本轮对话附件）：亮色 14px 一张、暗色 18px 一张（1900×1400，左右并排）。用户可直接打开 `compare.html` 亲验（右栏面板或独立窗口均可）。

## 6. 性能（node 实测，`gui-test/md-perf-test.js`）

| 场景 | 旧 | 新 | 说明 |
|---|---|---|---|
| 长文档 ~10KB（含表格/代码块/列表）单次 | 0.50ms | 2.14ms | 绝对值仍只有 2ms |
| 超长文档 ~54KB（超 32KB 流式降频档）单次 | 2.28ms | 12.12ms | 线性复杂度（10KB→2.1ms、54KB→12ms，无平方增长） |
| 流式 150 帧前缀重渲染（10KB 文档逐帧） | 0.24ms/帧 | 1.10ms/帧 | 降频档 150ms 一帧，1.1ms 占空比 ≈0.7%，无感 |
| 长对话 300 条小消息 | 0.8ms | 3.6ms | 合计毫秒级 |

结论：CPU 占比 4–5 倍但绝对值全部在毫秒级、复杂度线性，32KB/150ms 降频机制原样保留，无可感知卡顿。

## 7. 体积账（marked 全仓一份）

| | HEAD | 现在 |
|---|---|---|
| web/ 内嵌总量 | 1,410,790 B | 1,450,122 B（+marked.lib.js 35,596 B） |
| viewer-template.html | 48,786 B | 13,642 B（marked 换占位符） |
| **主 exe embed 合计** | 1,459,576 B | 1,463,764 B（**净增 +4,188 B**） |

marked 在主 exe 里仍然只 embed **一份**（`//go:embed web` 与 mdview.go 注入共用 webFS 的同一份字节）。若按「模板内联 + web 各放一份」的偷懒做法会净增 +35KB——已避开。另：模板占位符注入产物与 HEAD 的 `viewer-template.html` **逐字节一致**（`tpl-equiv` 验证通过），右栏 MD 查看器页面零变化；独立 `MD查看器.exe` 未重建（其内嵌资源内容等价，下次跑 build.ps1 自动走新注入流程）。

## 8. 测试汇总

- `node gui-test/md-render-test.js`：**83 pass, 0 fail**
- `node gui-test/linkify-test.js`：**16 pass, 0 fail**（含 XSS 一例）
- `go vet .`：通过
- `go test -run 'TestMDViewer|TestRenderMDViewer' .`：通过（注入测试在内）
- `go test ./...`：**通过**（主包 `ok jxassistant 128.488s`，其余三包无测试文件）
- 真实应用端到端冒烟：隔离实例（独立 dataDir + 端口 19097 + -webdir）实测 `marked` 加载、`md()` 各语法、`/marked.lib.js` 200、XSS 转义、CJK URL 截断——全部符合预期
- 隔离构建：`D:\智能体工作区\数字助理GO\坚祥数字助理GO.exe`（64,199,680 B，2026-11-15 15:38）

## 9. 已知行为差异（GFM 化的预期变化，非回归）

1. `文字\n---`（无空行）按 setext 渲染成标题（GFM/GitHub 行为；旧版当普通段落文字显示）。
2. 松散列表（项间空行）：项内文字包 `<p>`（GFM 标准），比旧的扁平列表稍疏；`1. a\n\n2. b` 旧版会重开编号（显示成两个 1.），新版正确连续编号。
3. `www.example.com` 现在自动成链接（GFM；旧版只认 http(s)://）。
4. 裸 URL 结尾的 ASCII 标点（`.` `,` 等）不再吞进链接（marked 的 backpedal；旧版会把句点吞进 href）。
5. 行内 HTML 仍按文字显示（与旧一致），但转义更规范（如源文本 `&amp;` 显示为 `&amp;` 而非双重转义）。
6. 4 空格缩进代码块现在渲染为代码块（GFM；旧版当普通段落）。
7. 旧渲染器的「粗体跨 `<a>` 标签误伤 href」潜伏缺陷（调研文档 §2.2）随架构更换自然消失。
