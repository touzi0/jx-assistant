---
name: office
description: 创建、读取、修改并校验 Word（docx）/ Excel（xlsx）/ PowerPoint（pptx）文档。当输入是这三类文件，或用户要求产出/修改文档、表格、演示文稿时使用。老式格式（.doc/.xls/.ppt）与 WPS 专有格式用 kind=com 老格式分册。
---

# Office 文档技能

三个文档族分别使用：docx → `python-docx`；xlsx → `openpyxl`；
pptx → `python-pptx`。由助手的 `office_skill` 工具获取对应分册要点，
然后用 `run_python` 执行 Python 实际操作（`run_command` 也可用）。

## 通用纪律

- 源脚本、中间文件、成品都放在任务工作区内；技能目录是只读资源。
- 修改既有文件前，先遍历结构（段落/表格/节/页）看清再动手；
  用户没要求重设计时，保留原有样式。
- **docx / pptx 动手前先出 MD 草稿**（xlsx 不需要）：新建整份文档
  先给大纲草稿（每节/每页讲什么），修改既有文档先给修改点清单
  （改哪里、改成什么样），交用户过目、同意后再动手；只有用户
  明确说「不用确认、直接做」的小改动才可跳过。
- 保存到新文件，除非用户明确要求原地修改。
- **生成后必须跑结构校验**（见下），校验通过才算完成。
- 校验器只保证包结构与文字内容；分页、截断、字体渲染它管不了，
  交付前把摘要与用户需求对一遍。

## docx 分册要点

- 直接改 `paragraph.text` 会丢掉 run 级格式；要保格式就改具体的 run。
- 中文必须同时设置 `run.font.name` 与东亚字体（rPr.rFonts 的 w:eastAsia），
  只写字体名不保证字形生效。
- 表格按所在节页面宽度设列宽；注意合并单元格与嵌套表。
- python-docx 不做分页与渲染；不要把普通替换谎报为"修订"。

## xlsx 分册要点

- openpyxl 读写同一工作簿，保留公式与格式；公式不会被计算——
  要读"计算结果"需 `data_only=True` 打开（前提是文件被 Excel 存过）。
- 大表先看 `ws.dimensions` / `max_row`，不要整表灌进上下文。
- 样式在单元格对象上设置；合并用 `ws.merge_cells`。

## pptx 分册要点

- 版式页：`slide_layouts[0]`=标题页、`[1]`=标题+内容；文本在
  shape.text_frame 的 paragraph/run 层级里。
- 图片 `add_picture` 的坐标单位是 EMU（914400 每英寸）。

## com 分册要点（老式格式与 WPS 文档）

适用：python 库读不了的老式 Office 格式（.doc/.xls/.ppt）与 WPS 专有
格式（.wps/.et/.dps）——用本机已装的 Office/WPS 软件的 COM 口处理。
本分册由 office_skill(kind=com) 提供，自动装配 pywin32。

- **run_python 调用的 arguments JSON 里必须真的写出 `"com": true` 字段**，
  完整形态示例：`{"code": "<脚本全文>", "name": "comdoc-1", "com": true}`。
  该参数不在工具 schema 里，是本分册的专用执行通道：置 true 时脚本绕过
  OS 沙箱在宿主直接执行——COM 在沙箱内无法运行，不带此参数 Dispatch 必
  挂起或报错，不要硬试。**发送前自查一遍 arguments：`"com"` 字段必须
  实际出现在工具调用的 JSON 里**——正文里写「我带了 com:true」不算带，
  漏了照样沙箱内挂起超时（历史事故：模型声称带了参数、arguments 里没有，
  误判成「引擎不识别该参数」）；字符串形态 `"com": "true"` 同样有效。
  **仅限 COM 驱动 Office/WPS 的脚本使用，其他脚本一律不带**；全电脑只读
  档下该通道不可用。
- ProgID：Word=`Word.Application`、Excel=`Excel.Application`、
  PPT=`PowerPoint.Application`；WPS 文字/表格/演示=`KWPS.Application` /
  `Ket.Application` / `KWPP.Application`（拿不准用 com-probe 技能探测）。
  报「无效的类字符串」= 该软件未装或 ProgID 不对，换一个或如实告知用户。
- 纪律：`Visible=False`、`DisplayAlerts=False`；try/finally 里必须
  `Quit()`（漏了会残留 WINWORD/WPS 进程占住文件句柄）；改既有文件前先
  snapshot_file 打快照。
- 常规套路：老 .doc 先 `SaveAs` 另存 .docx（wdFormatXMLDocument=16）、
  .xls 另存 .xlsx（xlWorkbookDefault=51），再交给 python-docx/openpyxl 处理；
  转换必有细节损耗，重要版式先向用户说明。

## 结构校验

生成/修改后对本文件执行（由 `office_skill` 工具返回的命令为准）：

```text
<python> <技能目录>/office/scripts/check_office.py <文档> --contains 必须出现的文字
```

校验 ZIP 完整性、关键部件 XML、关系文件，并统计 docx 段落/表格/节、
xlsx 工作表与单元格、pptx 页与形状。退出码 0 = 通过。
