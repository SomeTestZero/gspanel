// GLIBC 2.38 兼容垫片：让在 Ubuntu 24.04 构建的 libUE4SS.so 能在 glibc 2.35 上加载。
// 仅转发 6 个符号：fmod/fmodf/__isoc23_sscanf/__isoc23_strtol/__isoc23_strtoll/__isoc23_strtoull
#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>

// ---- fmod / fmodf：转发到系统 libm 的真实实现 ----
double fmod(double x, double y) {
    static double (*real)(double, double) = 0;
    if (!real) real = (double (*)(double, double))dlsym(RTLD_NEXT, "fmod");
    return real(x, y);
}
float fmodf(float x, float y) {
    static float (*real)(float, float) = 0;
    if (!real) real = (float (*)(float, float))dlsym(RTLD_NEXT, "fmodf");
    return real(x, y);
}

// ---- C23 变体：转发到 glibc 2.35 已有的实现（不处理 0b 前缀/新语义，够用）----
int __isoc23_sscanf(const char *s, const char *fmt, ...) {
    va_list ap;
    va_start(ap, fmt);
    int r = vsscanf(s, fmt, ap);
    va_end(ap);
    return r;
}
long __isoc23_strtol(const char *n, char **e, int b) { return strtol(n, e, b); }
long long __isoc23_strtoll(const char *n, char **e, int b) { return strtoll(n, e, b); }
unsigned long long __isoc23_strtoull(const char *n, char **e, int b) { return strtoull(n, e, b); }
