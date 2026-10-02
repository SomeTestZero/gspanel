#!/usr/bin/env python3
"""从 Palworld 服务端 pak 提取物品「内部 ID → 中文名」列表，生成面板内置基线。

背景：Palworld 1.0.4 的 DT_ItemDataTable 行结构体 PalStaticItemDataStruct 是精简表，
没有 Name 字段；显示名在 L10N/<lang>/Pal/DataTable/Text/DT_ItemNameText_Common 里
（行结构 PalLocalizedTextData，字段 TextData 是 FText，键为 ITEM_NAME_<物品ID>_TextData）。
游戏运行时用 UE4SS 读 FText 会直接 abort（见 README「已知坑」），所以中文名走离线提取。

用法:
    pip install repak
    # 先让面板/ mod 导出物品 ID 列表（gspanel-mod/items.json，队列命令 items）
    python3 extract-items-zh.py \
        /home/games/instances/palworld-1/Pal/Content/Paks/Pal-LinuxServer.pak \
        /home/games/instances/palworld-1/Pal/Binaries/Linux/gspanel-mod/items.json \
        ../../assets/palworld-items/palworld-zh.json

说明:
  * 只读 pak 里几个小 uexp（这些条目在 pak 里未压缩，不需要 Oodle）；
  * 名称回退链：zh-Hans → zh-Hant → en → 基础表 → 物品 ID；
  * 游戏更新后重跑本脚本、重新 build 面板即可刷新内置中文名。
"""
import datetime
import json
import os
import re
import struct
import sys

try:
    import repak
except ImportError:
    sys.exit("需要 repak：pip install repak")

NAME_TEXT_TABLE = "Pal/DataTable/Text/DT_ItemNameText_Common.uexp"
# 候选来源（按优先级）：L10N/<lang>/...，以及不带 L10N 的基础表（可能是日文源串）
SOURCES = [
    ("zh-Hans", "Pal/Content/L10N/zh-Hans/" + NAME_TEXT_TABLE),
    ("zh-Hant", "Pal/Content/L10N/zh-Hant/" + NAME_TEXT_TABLE),
    ("en", "Pal/Content/L10N/en/" + NAME_TEXT_TABLE),
    ("base", "Pal/Content/" + NAME_TEXT_TABLE),
]
# 游戏里未翻译项会写这种占位串
PLACEHOLDERS = {"", "-", "en text", "zh-hans text", "zh-hant text", "ja text", "ko text"}


def parse_name_table(data: bytes) -> dict:
    """解析 PalLocalizedTextData 文本表 uexp：ITEM_NAME_<id>_TextData → 名称。

    uexp 里每条记录形如：
        FString Namespace ("DT_ItemNameText_Common")
        FString Key       ("ITEM_NAME_Wood_TextData")
        FString Value     （长度为正 = ANSI；长度为负 = UTF-16LE，长度含结尾 \\0）
    """
    names = {}
    i = 0
    while True:
        i = data.find(b"ITEM_NAME_", i)
        if i < 0:
            break
        if i < 4:
            i += 1
            continue
        klen = struct.unpack_from("<i", data, i - 4)[0]
        if klen <= 0 or klen > 200:
            i += 1
            continue
        key = data[i:i + klen - 1].decode("latin1", "ignore")
        end = i + klen
        if not key.endswith("_TextData"):
            i += 1
            continue
        vlen = struct.unpack_from("<i", data, end)[0]
        pos = end + 4
        if vlen < 0:  # UTF-16
            n = -vlen
            val = data[pos:pos + n * 2][:-2].decode("utf-16-le", "ignore") if n > 0 else ""
        elif vlen > 0:  # ANSI/UTF-8
            val = data[pos:pos + vlen][:-1].decode("utf-8", "ignore")
        else:
            val = ""
        item_id = key[len("ITEM_NAME_"):-len("_TextData")]
        if item_id:
            names[item_id] = val
        i = end
    return names


def valid(name: str) -> bool:
    return bool(name) and name.strip().lower() not in PLACEHOLDERS


def main() -> None:
    if len(sys.argv) < 4:
        sys.exit(__doc__)
    pak_path, ids_path, out_path = sys.argv[1:4]

    with open(ids_path, encoding="utf-8") as f:
        ids = json.load(f)["ids"]

    reader = repak.PakBuilder().reader(pak_path)
    print("pak:", pak_path, "version:", reader.version, "entries:", len(reader.files()))
    langs = {}
    for lang, path in SOURCES:
        try:
            langs[lang] = parse_name_table(reader.get(path))
            print(f"  {lang}: {len(langs[lang])} 条 <- {path}")
        except Exception as e:  # noqa: BLE001
            print(f"  {lang}: 读取失败 {type(e).__name__}: {e}")

    stats = {}
    items = []
    for item_id in ids:
        name, src = "", "id"
        for lang, _ in SOURCES:
            v = langs.get(lang, {}).get(item_id, "")
            if valid(v):
                name, src = v, lang
                break
        stats[src] = stats.get(src, 0) + 1
        items.append({"id": item_id, "name": name or item_id})

    # 构建号从实例 appmanifest 读（从 items.json 路径向上找 steamapps/）
    build = ""
    cur = os.path.dirname(os.path.abspath(ids_path))
    for _ in range(6):
        steamapps = os.path.join(cur, "steamapps")
        if os.path.isdir(steamapps):
            try:
                acf = next(f for f in os.listdir(steamapps) if f.startswith("appmanifest_") and f.endswith(".acf"))
                build = re.search(r'"buildid"\s+"(\d+)"', open(os.path.join(steamapps, acf)).read()).group(1)
            except Exception:  # noqa: BLE001
                pass
            break
        parent = os.path.dirname(cur)
        if parent == cur:
            break
        cur = parent

    db = {
        "source": "内置基线（从游戏 pak 提取：L10N/zh-Hans 为主，缺翻译回退 zh-Hant/en）",
        "culture": "zh-Hans",
        "generated_at": datetime.datetime.now().astimezone().isoformat(timespec="seconds"),
        "game_build_id": build,
        "count": len(items),
        "items": items,
    }
    with open(out_path, "w", encoding="utf-8") as f:
        json.dump(db, f, ensure_ascii=False, indent=1)
    print("写出:", out_path)
    print("名称来源统计:", stats)


if __name__ == "__main__":
    main()
