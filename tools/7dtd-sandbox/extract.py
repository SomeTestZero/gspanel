#!/usr/bin/env python3
"""从 7 Days to Die 专用服务器文件提取沙盒选项表（SandboxCode 编辑器用）。

游戏更新后重跑即可刷新 assets/7dtd-sandbox/options.json：

    python3 extract.py /home/games/instances/7days /home/ubuntu/gspanel/assets/7dtd-sandbox/options.json

数据来源（全部来自游戏本体，无手工维护）：
  1. 7DaysToDieServer_Data/Managed/Assembly-CSharp.dll
     - `SandboxOptions.SandboxOptions` 枚举声明序 = SandboxCode 的选项 ID（2 字符 base-26）
     - `SandboxOptionManager.SetupOptions` IL：每个选项的注册参数（显示名/分类/值集/默认值）
       与每个值集的取值数组（DisplayValues/DisplayFormat/FloatValues/IntValues/BoolValues）
  2. Data/Config/Localization.csv：选项名/描述/值标签的中文文案（go* 键）

SandboxCode 编码格式（与游戏 `IndexToAlpha2`/`AlphaToIndex` 一致）：
  "A" + 若干 3 字符块；块 = 2 字符 base-26 选项 ID + 1 字符值索引（'A'=0）；默认值项省略。
"""
import csv
import hashlib
import json
import os
import struct
import sys

import dnfile
from dncil.cil.body import CilMethodBody
from dncil.cil.body.reader import CilMethodBodyReaderBytes

ASM_REL = "7DaysToDieServer_Data/Managed/Assembly-CSharp.dll"
LOC_REL = "Data/Config/Localization.csv"

# GetDisplayAtIndex 里用 DisplayFormat 本地化键做数值格式化；百分比按 value*100 显示
FORMAT_SCALE = {"goPercent": 100.0}

PUSH_OPS = {
    "ldstr", "ldnull", "ldtoken",
    "ldc.i4", "ldc.i4.s", "ldc.i4.m1", "ldc.i4.0", "ldc.i4.1", "ldc.i4.2", "ldc.i4.3",
    "ldc.i4.4", "ldc.i4.5", "ldc.i4.6", "ldc.i4.7", "ldc.i4.8",
    "ldc.r4", "ldc.r8",
}
CONST_I4 = {f"ldc.i4.{i}": i for i in range(9)}
CONST_I4.update({"ldc.i4.m1": -1})

VALUE_FIELDS = {"DisplayValues", "AlternateDisplayValues", "FloatValues", "IntValues", "BoolValues"}
SET_TYPES = {"SandboxOptionValueSet", "SandboxOptionValueSetFloat", "SandboxOptionValueSetInt", "SandboxOptionValueSetBool"}
OPT_TYPES = {"SandboxOptionFloat", "SandboxOptionInt", "SandboxOptionBoolean"}


def blob_bytes(item):
    v = item.value_bytes() if callable(item.value_bytes) else item.value_bytes
    return bytes(v)


def compressed_uint(data, pos):
    b = data[pos]
    if b < 0x80:
        return b, pos + 1
    if b < 0xC0:
        return ((b & 0x3F) << 8) | data[pos + 1], pos + 2
    return ((b & 0x1F) << 24) | (data[pos + 1] << 16) | (data[pos + 2] << 8) | data[pos + 3], pos + 4


def sig_param_count(pe, method_row):
    sig = method_row.Signature
    blob = blob_bytes(sig)
    pos = 1  # 调用约定字节
    n, _ = compressed_uint(blob, pos)
    return n


class Ctx:
    def __init__(self, asm_path):
        self.pe = dnfile.dnPE(asm_path)
        pe = self.pe
        # Field 名字(混淆哈希) -> Field 行；FieldRva: Field 行 -> 原始数据 RVA
        self.field_by_name = {}
        for row in pe.net.mdtables.Field.rows:
            self.field_by_name[str(row.Name)] = row
        self.field_rva = {}
        for row in pe.net.mdtables.FieldRva.rows:
            self.field_rva[id(row.Field.row)] = row.Rva
        # MethodDef 行 -> 所属类型名
        self.method_owner = {}
        for tdef in pe.net.mdtables.TypeDef.rows:
            for mi in tdef.MethodList:
                self.method_owner[id(mi.row)] = (str(tdef.TypeNamespace or ""), str(tdef.TypeName))

    def resolve(self, token):
        table, rid = (token >> 24) & 0xFF, token & 0xFFFFFF
        if table == 0x70:
            s = self.pe.net.user_strings.get(rid)
            return ("str", s.value if s else "")
        if table == 0x06:
            row = self.pe.net.mdtables.MethodDef.rows[rid - 1]
            owner = self.method_owner.get(id(row), ("", ""))
            return ("method", (owner[1], str(row.Name), row))
        if table == 0x0A:
            row = self.pe.net.mdtables.MemberRef.rows[rid - 1]
            cls = row.Class.row
            owner = str(cls.TypeName) if cls is not None and hasattr(cls, "TypeName") else ""
            return ("method", (owner, str(row.Name), row))
        if table == 0x04:
            row = self.pe.net.mdtables.Field.rows[rid - 1]
            return ("field", str(row.Name))
        if table in (0x01, 0x02):
            row = (self.pe.net.mdtables.TypeRef.rows if table == 0x01 else self.pe.net.mdtables.TypeDef.rows)[rid - 1]
            return ("type", str(row.TypeName))
        return ("token", hex(token))

    def field_blob(self, field_name, elem_type, count):
        row = self.field_by_name.get(field_name)
        if row is None:
            raise KeyError(field_name)
        rva = self.field_rva[id(row)]
        fmt = {"Single": "<f", "Double": "<d", "Int32": "<i", "Boolean": "<B"}[elem_type]
        size = struct.calcsize(fmt) * count
        raw = self.pe.get_data(rva, size)
        return [struct.unpack(fmt, raw[i * struct.calcsize(fmt):(i + 1) * struct.calcsize(fmt)])[0]
                for i in range(count)]


def find_type(pe, name, ns=""):
    for row in pe.net.mdtables.TypeDef.rows:
        if str(row.TypeName) == name and str(row.TypeNamespace or "") == ns:
            return row
    raise KeyError(name)


def lit_value(ctx, ins):
    """字面量 push 指令 -> python 值"""
    op = ins.opcode.name
    if op == "ldstr":
        return ("str", ctx.resolve(ins.operand.value)[1] if ins.operand is not None else "")
    if op == "ldnull":
        return ("null", None)
    if op == "ldtoken":
        return ("blobfield", ctx.resolve(ins.operand.value)[1])
    if op in CONST_I4:
        return ("int", CONST_I4[op])
    if op == "ldc.i4" or op == "ldc.i4.s":
        return ("int", int(ins.operand.value if hasattr(ins.operand, "value") else ins.operand))
    if op == "ldc.r4":
        v = ins.operand.value if hasattr(ins.operand, "value") else ins.operand
        return ("float", float(struct.unpack("<f", struct.pack("<f", float(v)))[0]))
    if op == "ldc.r8":
        v = ins.operand.value if hasattr(ins.operand, "value") else ins.operand
        return ("float", float(v))
    raise ValueError(op)


def walk_setup_options(ctx):
    """扫 SetupOptions 的 IL，重建值集与选项注册表"""
    t = find_type(ctx.pe, "SandboxOptionManager", "SandboxOptions")
    m = [x.row for x in t.MethodList if str(x.row.Name) == "SetupOptions"][0]
    body = CilMethodBody(CilMethodBodyReaderBytes(ctx.pe.get_data(m.Rva, 0x40000)))
    insns = body.instructions

    value_sets = {}   # name -> dict
    options = []      # 注册顺序
    lits = []         # 连续字面量 push
    cur_set = None
    cur_arr = None
    pending_name = None
    cur_opt = None

    for ins in insns:
        op = ins.opcode.name
        if op in PUSH_OPS:
            lits.append(lit_value(ctx, ins))
            continue
        if op == "newarr":
            n = lits.pop()[1]
            elem = ctx.resolve(ins.operand.value)[1]
            cur_arr = {"elem": elem.rsplit(".", 1)[-1], "len": int(n), "items": {}, "blob": None}
            lits.clear()
            continue
        if op in ("dup",):
            continue
        if op.startswith("stelem"):
            val = lits.pop()
            idx = lits.pop()[1]
            cur_arr["items"][int(idx)] = val
            continue
        if op == "call" and ctx.resolve(ins.operand.value)[1][1] == "InitializeArray":
            blob = [v for v in lits if v[0] == "blobfield"]
            cur_arr["blob"] = blob[-1][1]
            lits.clear()
            continue
        if op == "stfld":
            fname = ctx.resolve(ins.operand.value)[1]
            if fname in VALUE_FIELDS and cur_arr is not None:
                (cur_set or cur_opt)["fields"][fname] = cur_arr
                cur_arr = None
            elif cur_set is not None:
                cur_set["fields"][fname] = lits[-1] if lits else None
            elif cur_opt is not None:
                cur_opt["fields"][fname] = lits[-1] if lits else None
            lits.clear()
            continue
        if op == "newobj":
            owner, name, row = ctx.resolve(ins.operand.value)[1]
            argc = sig_param_count(ctx.pe, row)
            args = lits[len(lits) - argc:] if argc else []
            if owner in SET_TYPES:
                key = lits[-(argc + 1)][1] if lits else None  # newobj 前的字典键
                cur_set = {"name": key, "type": owner, "fields": {}}
                if key is not None:
                    value_sets[key] = cur_set
            elif owner in OPT_TYPES:
                cur_opt = {
                    "opt_type": owner,
                    "id": args[0][1] if args else None,
                    "ui_name": args[1][1] if len(args) > 1 else None,
                    "category": args[2][1] if len(args) > 2 else None,
                    "valueset": args[3][1] if len(args) > 3 else None,
                    "default": args[4] if len(args) > 4 else None,
                    "fields": {},
                }
            lits.clear()
            continue
        if op == "callvirt" and ctx.resolve(ins.operand.value)[1][1] == "Add":
            cur_set = None
            lits.clear()
            continue
        if op == "call" and ctx.resolve(ins.operand.value)[1][1] == "AddSandboxOption":
            if cur_opt:
                options.append(cur_opt)
            cur_opt = None
            lits.clear()
            continue
        if op.startswith("ldarg") or op.startswith("ldloc") or op.startswith("ldfld") \
                or op.startswith("ldsfld"):
            # 非字面量 push：占位保留（部分选项构造参数是局部变量）
            lits.append(("opaque", None))
            continue
        # 其余指令（分支/比较等）保守清空
        lits.clear()

    return value_sets, options


def unpack_values(ctx, vs):
    """值集 -> [(raw_value, display_key_or_None)]"""
    f = vs["fields"]
    arr = None
    for key in ("FloatValues", "IntValues", "BoolValues"):
        if key in f:
            arr = f[key]
            break
    if arr is None:
        return []
    vals = [None] * arr["len"]
    if arr["blob"]:
        raw = ctx.field_blob(arr["blob"], arr["elem"], arr["len"])
        vals = [(bool(v) if arr["elem"] == "Boolean" else v) for v in raw]
    else:
        # 字段内联初始化：未显式填充的元素是类型零值
        zero = {"Single": 0.0, "Double": 0.0, "Int32": 0, "Boolean": False}.get(arr["elem"], None)
        vals = [zero] * arr["len"]
        for i, v in arr["items"].items():
            vals[i] = v[1]
    if arr["elem"] == "Boolean":
        vals = [bool(v) for v in vals]
    displays = {}
    for dkey in ("DisplayValues", "AlternateDisplayValues"):
        if dkey in f and dkey == "DisplayValues":
            for i, v in f[dkey]["items"].items():
                displays[i] = v[1]
    return [(vals[i], displays.get(i)) for i in range(arr["len"])]


def load_loc(path):
    loc = {}
    with open(path, newline="", encoding="utf-8-sig") as fh:
        rd = csv.reader(fh)
        header = next(rd)
        i_en = header.index("english")
        i_zh = header.index("schinese")
        for row in rd:
            if len(row) > max(i_en, i_zh):
                loc[row[0]] = {"en": row[i_en], "zh": row[i_zh]}
    return loc


def loc_get(loc, key):
    if not key:
        return None
    e = loc.get(key)
    if not e:
        return None
    return e["zh"] or e["en"]


def num_str(v):
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, float) and v == int(v):
        return str(int(v))
    return f"{v:g}"


def value_label(loc, raw, display, fmt_key):
    if display is not None:
        s = loc_get(loc, display) or display
        if "{0}" in s:  # 值标签自身带占位符（如 goDay）
            s = s.replace("{0}", num_str(raw))
        return s
    if fmt_key:
        tpl = loc_get(loc, fmt_key) or "{0}"
        v = raw * FORMAT_SCALE.get(fmt_key, 1.0)
        return tpl.replace("{0}", num_str(v))
    return num_str(raw)


def main():
    inst_dir = sys.argv[1]
    out_path = sys.argv[2]
    asm = os.path.join(inst_dir, ASM_REL)
    loc_path = os.path.join(inst_dir, LOC_REL)

    ctx = Ctx(asm)
    value_sets, opt_regs = walk_setup_options(ctx)

    enum_row = find_type(ctx.pe, "SandboxOptions", "SandboxOptions")
    enum_names = [str(f.row.Name) for f in enum_row.FieldList
                  if str(f.row.Name) not in ("value__", "Max")]
    # 枚举常量值（防非连续）
    enum_values = {}
    for c in ctx.pe.net.mdtables.Constant.rows:
        parent = c.Parent.row
        if parent is not None and id(parent) in {id(f.row) for f in enum_row.FieldList}:
            enum_values[str(parent.Name)] = int.from_bytes(blob_bytes(c.Value), "little", signed=True)

    loc = load_loc(loc_path)
    options = []
    problems = []
    for reg in opt_regs:
        if reg["id"] is None:
            problems.append(f"缺 id: {reg}")
            continue
        name = enum_names[reg["id"]] if reg["id"] < len(enum_names) else f"opt{reg['id']}"
        vs = value_sets.get(reg["valueset"])
        if vs is None:
            problems.append(f"{name}: 值集缺失 {reg['valueset']}")
            continue
        raw_vals = unpack_values(ctx, vs)
        fmt_key = vs["fields"].get("DisplayFormat")
        fmt_key = fmt_key[1] if fmt_key else None
        default_raw = reg["default"][1] if reg["default"] else None
        default_idx = None
        values = []
        for i, (raw, disp) in enumerate(raw_vals):
            values.append({"raw": raw, "label": value_label(loc, raw, disp, fmt_key)})
            if default_raw is not None and raw is not None and not isinstance(default_raw, str) \
                    and not isinstance(raw, str) and abs(float(raw) - float(default_raw)) < 1e-9:
                default_idx = i
        if default_idx is None:
            problems.append(f"{name}: 默认值 {default_raw!r} 不在值集 {reg['valueset']} 中")
            default_idx = 0
        label_key = reg["fields"].get("OverrideOptionName")
        desc_key = reg["fields"].get("OverrideDescriptionName")
        label_key = label_key[1] if label_key else f"go{name}"
        desc_key = desc_key[1] if desc_key else f"go{name}Desc"
        options.append({
            "id": reg["id"],
            "name": name,
            "label": loc_get(loc, label_key) or reg["ui_name"] or name,
            "desc": loc_get(loc, desc_key) or "",
            "category": reg["category"],
            "values": values,
            "default": default_idx,
        })

    options.sort(key=lambda o: o["id"])
    cats = []
    for o in options:
        if o["category"] not in cats:
            cats.append(o["category"])
    categories = [{"key": c, "label": loc_get(loc, f"sandboxOptionCategory{c}") or c} for c in cats]

    with open(asm, "rb") as fh:
        sha = hashlib.sha256(fh.read()).hexdigest()

    doc = {
        "id": "7dtd-v3.2",
        "source": f"Assembly-CSharp.dll sha256:{sha[:16]}",
        "option_count": len(options),
        "categories": categories,
        "options": options,
    }
    os.makedirs(os.path.dirname(out_path), exist_ok=True)
    with open(out_path, "w", encoding="utf-8") as fh:
        json.dump(doc, fh, ensure_ascii=False, indent=1)

    print(f"选项 {len(options)} 个，值集 {len(value_sets)} 个 -> {out_path}")
    if problems:
        print("问题：")
        for p in problems:
            print(" -", p)
        sys.exit(1)
    # 快速自检：枚举序与 ID 一致
    for o in options:
        assert enum_values.get(o["name"]) == o["id"], (o["name"], o["id"])


if __name__ == "__main__":
    main()
