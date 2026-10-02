// gxxfix —— 进程级 __gxx_personality_v0 拦截垫片（Palworld 原生 Linux 专用）
//
// 背景：PalServer 的 libsteam_api.so 静态链接了一份旧 libstdc++，并把
// __gxx_personality_v0 以**无版本**形式导出。动态链接器按全局符号序解析时，
// LD_PRELOAD 进来的 libUE4SS.so（以及游戏自身）里所有 C++ 帧的 personality
// 引用（@CXXABI_1.3）都会绑定到这个旧副本上。该副本与系统 libgcc_s 的
// unwinder 不兼容：一旦有 C++ 异常需要 unwind（例如 UE4SS Lua 绑定层抛出
// std::runtime_error，本来会被 TRY/catch 正常捕获），它在安装 landing pad 时
// 直接 abort() → SIGABRT 杀掉整个游戏进程（systemd 崩溃循环）。
//
// 本垫片以 LD_PRELOAD 方式抢在 libsteam_api 之前导出一个正常工作的
// __gxx_personality_v0：启动时从系统 libstdc++.so.6 取出真身（CXXABI_1.3），
// 之后所有 personality 调用都转发给它，C++ 异常恢复正常的捕获语义。
// 对游戏自身也是修复而非降级（真身就是该符号的正确实现）。
//
// 只导出这一个符号（见 gxxfix.map），不干预其他符号解析。
#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdio.h>
#include <unwind.h>

typedef _Unwind_Reason_Code (*personality_fn)(int, _Unwind_Action,
                                              unsigned long long,
                                              struct _Unwind_Exception *,
                                              struct _Unwind_Context *);

static personality_fn real_personality;

// 构造函数在游戏主函数前运行（此时还没多线程/unwind），安全地做 dlopen/dlvsym。
// 注意不能在 personality 被调用时才惰性解析：unwind 途中拿加载器锁可能死锁。
__attribute__((constructor)) static void gxxfix_init(void)
{
    // libstdc++.so.6 已被 libUE4SS.so 依赖加载；dlopen 只是取句柄，不会重复加载。
    void *h = dlopen("libstdc++.so.6", RTLD_NOW | RTLD_NOLOAD);
    if (!h)
        h = dlopen("libstdc++.so.6", RTLD_NOW | RTLD_LOCAL);
    if (h)
    {
        real_personality = (personality_fn)dlvsym(h, "__gxx_personality_v0", "CXXABI_1.3");
        if (!real_personality)
            real_personality = (personality_fn)dlsym(h, "__gxx_personality_v0");
    }
    if (real_personality)
        fprintf(stderr, "[gxxfix] __gxx_personality_v0 interposed -> libstdc++ (%p)\n",
                (void *)real_personality);
    else
        fprintf(stderr, "[gxxfix] WARN: libstdc++ __gxx_personality_v0 not found, unwind 将按 continue 处理\n");
}

// 显式定义（无版本）。ELF 规则：无版本定义可以满足带版本（@CXXABI_1.3）的引用，
// 且本垫片在 LD_PRELOAD 序列中位于 libsteam_api 之前，优先级最高。
_Unwind_Reason_Code __gxx_personality_v0(int version, _Unwind_Action actions,
                                         unsigned long long exception_class,
                                         struct _Unwind_Exception *ue_header,
                                         struct _Unwind_Context *context)
{
    if (real_personality)
        return real_personality(version, actions, exception_class, ue_header, context);
    // 兜底：真身缺席时按「本帧无法处理」继续 unwind，无论如何不 abort。
    return _URC_CONTINUE_UNWIND;
}
