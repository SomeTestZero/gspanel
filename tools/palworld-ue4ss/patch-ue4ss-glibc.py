#!/usr/bin/env python3
"""把 Ubuntu 24.04 构建的 libUE4SS.so 的 GLIBC_2.38 版本需求降级为 GLIBC_2.2.5。

背景：ue4ss-linux 的预编译 libUE4SS.so 在 Ubuntu 24.04 (glibc 2.38) 上构建，
在 Ubuntu 22.04 (glibc 2.35) 上加载会报：
    version `GLIBC_2.38' not found (required by libUE4SS.so)
它实际只需要 6 个 2.38 符号：fmod/fmodf/__isoc23_sscanf/__isoc23_strtol/
__isoc23_strtoll/__isoc23_strtoull（见 shim/shim.c）。glibc 的动态链接器要求
版本必须由 VERNEED 里登记的那个库（libm.so.6 / libc.so.6）自己提供，
所以仅靠 LD_PRELOAD 垫片无法通过校验；把 VERNEED 的名称就地改成
GLIBC_2.2.5（libm/libc 都有）后，垫片按 GLIBC_2.2.5 导出的 6 个符号即可接管。

用法：
    python3 patch-ue4ss-glibc.py <libUE4SS.so>            # 就地打补丁（自动备份 .bak）
    python3 patch-ue4ss-glibc.py <libUE4SS.so> --dry-run  # 只检查

同时需要 jammy 版的新 libstdc++（GLIBCXX_3.4.31/32）：
    ubuntu-toolchain-r/test PPA 的 libstdc++6（GCC 16 for jammy），见 README。
"""
import re
import subprocess
import sys


def dynstr_range(path):
    out = subprocess.check_output(["readelf", "-SW", path], text=True)
    # 形如: [ 5] .dynstr STRTAB 000f8900 000f8900 0039f6dd ...
    m = re.search(
        r"\[\s*\d+\]\s+\.dynstr\s+STRTAB\s+([0-9a-f]+)\s+([0-9a-f]+)\s+([0-9a-f]+)",
        out,
    )
    if not m:
        raise SystemExit("找不到 .dynstr section")
    _addr, off, size = (int(x, 16) for x in m.groups())
    return off, size


def main():
    if len(sys.argv) < 2:
        raise SystemExit(__doc__)
    path = sys.argv[1]
    dry = "--dry-run" in sys.argv
    off, size = dynstr_range(path)
    data = bytearray(open(path, "rb").read())
    seg = bytes(data[off : off + size])
    n = seg.count(b"GLIBC_2.38")
    print(f".dynstr off=0x{off:x} size=0x{size:x} GLIBC_2.38 x{n}")
    if n == 0:
        print("无需处理（已经是 GLIBC_2.2.5 或不是同一构建）")
        return
    if dry:
        return
    open(path + ".bak", "wb").write(data)
    new = seg.replace(b"GLIBC_2.38", b"GLIBC_2.2.5")
    data[off : off + size] = new
    open(path, "wb").write(data)
    print(f"已打补丁：{path}（备份 {path}.bak）")


if __name__ == "__main__":
    main()
