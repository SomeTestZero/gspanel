package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// 模板路径项（backup_paths / world_paths）语法：
//   - 相对路径：相对实例目录（原有行为）
//   - "~/..."：games 用户家目录（GamesHome）下——部分游戏把世界存档写到 $HOME/.local/share/...
//     （七日杀 V3.2 即如此），不在实例目录里，备份/删世界都必须够得到
//   - "{config:配置键}"：按实例配置文件当前值展开（world_paths 用来定位「当前世界」存档目录，
//     如 Saves/{config:GameWorld}/{config:GameName}）
//   - "!模式"（仅 backup_paths）：tar --exclude 排除模式（按归档内成员名匹配，* 含 /），
//     用来把 mod 客户端资产等大块内容挡在备份外
// 禁止 ".." 段，禁止 ~/ 之外的绝对路径；{config:} 的展开值同样过校验（配置值不能把路径带飞）。
//
// 备份/恢复的 tar 兼容旧格式：实例外路径以绝对名进归档（tar -P 保留前导 /），
// 实例内仍是相对名，恢复时相对名解到实例目录、绝对名解到原位置。

var configPlaceholderRe = regexp.MustCompile(`\{config:([A-Za-z0-9_.-]+)\}`)

// validatePathSpec 校验模板路径项语法
func validatePathSpec(spec string) error {
	if spec == "" {
		return fmt.Errorf("路径不能为空")
	}
	if strings.Contains(spec, "\x00") {
		return fmt.Errorf("路径非法: %q", spec)
	}
	for _, seg := range strings.Split(spec, "/") {
		if seg == ".." {
			return fmt.Errorf("路径不允许包含 ..: %q", spec)
		}
	}
	if strings.HasPrefix(spec, "/") {
		return fmt.Errorf("路径须为相对实例目录或以 ~/ 开头: %q", spec)
	}
	// 占位符之外不允许残留花括号，避免写错语法后被当字面量静默匹配
	if strings.Contains(configPlaceholderRe.ReplaceAllString(spec, ""), "{") ||
		strings.Contains(configPlaceholderRe.ReplaceAllString(spec, ""), "}") {
		return fmt.Errorf("路径占位符语法非法（仅支持 {config:配置键}）: %q", spec)
	}
	return nil
}

// templateConfigValues 汇总实例各配置文件的当前值（后声明的 spec 覆盖先声明的）。
// 配置文件不存在/不可读的跳过——查不到具体键时由调用方报错。
func templateConfigValues(inst *Instance, tmpl *GameTemplate) map[string]string {
	values := map[string]string{}
	for i := range tmpl.Configs {
		kv, _, err := readConfigFile(inst.Dir, &tmpl.Configs[i])
		if err != nil {
			continue
		}
		for k, v := range kv {
			values[k] = v
		}
	}
	return values
}

// expandConfigPlaceholders 展开 {config:Key}；键无值时报错——宁可失败也不能按错路径删/打包
func expandConfigPlaceholders(inst *Instance, tmpl *GameTemplate, spec string) (string, error) {
	if !configPlaceholderRe.MatchString(spec) {
		return spec, nil
	}
	values := templateConfigValues(inst, tmpl)
	var missing []string
	out := configPlaceholderRe.ReplaceAllStringFunc(spec, func(m string) string {
		key := configPlaceholderRe.FindStringSubmatch(m)[1]
		v := strings.TrimSpace(values[key])
		if v == "" {
			missing = append(missing, key)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("配置键 %s 无值，无法解析路径 %q", strings.Join(missing, "/"), spec)
	}
	return out, nil
}

// resolveTemplatePath 把模板路径项解析为绝对路径（生产环境 games 家目录 = GamesHome）
func resolveTemplatePath(inst *Instance, tmpl *GameTemplate, spec string) (string, error) {
	return resolvePathSpec(GamesHome, inst, tmpl, spec)
}

// resolvePathSpec 同 resolveTemplatePath，home 供测试注入
func resolvePathSpec(home string, inst *Instance, tmpl *GameTemplate, spec string) (string, error) {
	if err := validatePathSpec(spec); err != nil {
		return "", err
	}
	p, err := expandConfigPlaceholders(inst, tmpl, spec)
	if err != nil {
		return "", err
	}
	if err := validatePathSpec(p); err != nil {
		return "", fmt.Errorf("路径 %q 展开后非法: %v", spec, err)
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(p, "~/"))), nil
	}
	return filepath.Join(inst.Dir, filepath.FromSlash(p)), nil
}

// templateOutsidePaths 模板路径项里解析到实例外的绝对路径（备份恢复后要 chown 回 games）
func templateOutsidePaths(inst *Instance, tmpl *GameTemplate) map[string]bool {
	out := map[string]bool{}
	all := append(append([]string(nil), tmpl.BackupPaths...), tmpl.WorldPaths...)
	for _, spec := range all {
		abs, err := resolveTemplatePath(inst, tmpl, spec)
		if err != nil || strings.HasPrefix(abs, inst.Dir+"/") || abs == inst.Dir {
			continue
		}
		out[abs] = true
	}
	return out
}
