#!/bin/sh
# Linux 发行包 WSL 侧组装器（由 打包-linux.ps1 调用；也可手工跑）。
#
# 为什么需要这一层：发行包必须在 Linux 侧打 tar——Windows bsdtar 从 NTFS
# 打包会丢执行位与 symlink，装出去的 python3 打不开。仓库里的运行时副本
# 因 NTFS 限制是「symlink 退化成内容=目标的文本文件」形态（robocopy 对
# LX_SYMLINK reparse point 直接 ERROR 1920，与 macos 运行时同款处理），
# bin 下关键的别名链接在这里重建为真 symlink。
#
# 用法: sh pkg-prepare.sh <stage目录(WSL路径)> <tar.gz输出(WSL路径)> <1=带运行时>
# 产出条目（tar.gz 平铺于根）：jx-assistant-linux-amd64 / README-Linux.md /
#   jx-assistant.service / 技能/（内置技能 + _runtime/linux 便携运行时）
set -e
STAGE="$1"; OUT="$2"; WITH_RT="$3"
[ -d "$STAGE" ] || { echo "stage 目录不存在: $STAGE" >&2; exit 1; }

ROOT="$HOME/pkg-root"
rm -rf "$ROOT"
mkdir -p "$ROOT/技能/_runtime"

cp -a "$STAGE/jx-assistant-linux-amd64" "$ROOT/"
cp -a "$STAGE/README-Linux.md" "$ROOT/"
cp -a "$STAGE/jx-assistant.service" "$ROOT/"

# 内置技能（自枚举，跳过 _runtime；stage 里没有 user——打包脚本就不拷）
for d in "$STAGE/技能"/*; do
  [ -e "$d" ] || continue
  name=$(basename "$d")
  [ "$name" = "_runtime" ] && continue
  cp -a "$d" "$ROOT/技能/$name"
done

ENTRIES="jx-assistant-linux-amd64 README-Linux.md jx-assistant.service"

if [ "$WITH_RT" = "1" ]; then
  RT="$ROOT/技能/_runtime/linux"
  [ -f "$STAGE/技能/_runtime/linux/bin/python3.14" ] \
    || { echo "stage 缺 _runtime/linux/bin/python3.14" >&2; exit 1; }
  cp -a "$STAGE/技能/_runtime/linux" "$RT"

  # 重建 bin 下别名链接：bin 里 ≤64 字节、内容指向同目录存在文件的「文本」
  # 就是退化的 symlink（真实场景只有 bin 下需要链接形态；terminfo 一类的
  # 链接不参与 Python 运行，保持文本形态无害）。
  cd "$RT/bin"
  for f in *; do
    [ -f "$f" ] || continue
    sz=$(stat -c%s "$f" 2>/dev/null) || continue
    [ "$sz" -lt 64 ] || continue
    t=$(cat "$f" 2>/dev/null) || continue
    if [ -n "$t" ] && [ -f "$t" ] && [ "$t" != "$f" ]; then
      rm -f "$f"; ln -s "$t" "$f"
    fi
  done
  cd - >/dev/null

  # 依赖 import 校验（缺一不出包——对齐 Windows 打包「自包含」口径）
  "$RT/bin/python3" -c 'import rapidocr_onnxruntime, pypdfium2, pdfplumber, pypdf, docx, openpyxl, pptx, numpy, cv2; print("IMPORTS-OK")' \
    || { echo "stage 运行时 import 校验失败" >&2; exit 1; }
  echo "IMPORTS-OK"

  # 权限规整：目录 755、普通文件 644、运行时 bin 与主程序 755
  find "$ROOT" -type d -exec chmod 0755 {} +
  find "$ROOT" -type f -exec chmod 0644 {} +
  find "$RT/bin" -maxdepth 1 -type f -exec chmod 0755 {} +
  chmod 0755 "$ROOT/jx-assistant-linux-amd64"
  ENTRIES="$ENTRIES 技能"
fi

chmod 0755 "$ROOT/jx-assistant-linux-amd64"

rm -f "$OUT"
# shellcheck disable=SC2086
tar -czf "$OUT" -C "$ROOT" $ENTRIES

# 包体自检：关键条目 + python3 执行位（装出去要能直接跑）
L=$(tar -tzf "$OUT")
echo "$L" | grep -q '^jx-assistant-linux-amd64$' || { echo "包自检失败：缺主程序" >&2; exit 1; }
if [ "$WITH_RT" = "1" ]; then
  for m in '技能/_runtime/linux/bin/python3.14' '技能/_runtime/linux/bin/python3' \
           '技能/pdf/manifest.json' '技能/ocr/manifest.json' '技能/office/manifest.json'; do
    echo "$L" | grep -q "^$m$" || { echo "包自检失败：缺 $m" >&2; exit 1; }
  done
  mode=$(tar -tvzf "$OUT" | awk '$NF ~ /bin\/python3\.14$/ {print $1; exit}')
  case "$mode" in
    -rwxr-xr-x) ;;
    *) echo "包自检失败：python3.14 执行位异常: $mode" >&2; exit 1 ;;
  esac
fi
rm -rf "$ROOT"
echo PKG-PREPARE-OK
