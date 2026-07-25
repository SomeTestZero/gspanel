package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ---------- 游戏配置文件读写：option-settings（Palworld）/ kv / raw ----------
//
// Palworld v1.0 的 OptionSettings 解析器（UE 属性解析）要求：
//   - 字符串值必须加引号:  ServerName="xxx"；布尔/数值不加引号: RCONEnabled=True
//   - 文件为 CRLF 换行并带注释头，写入时必须保留原格式
// 因此写入采用「就地替换」，保留每个键原有的引号风格，不重排整个文件。

var optionLineRe = regexp.MustCompile(`(?s)OptionSettings=\((.*)\)`)

// parseOptionSettings 解析 Palworld 风格: OptionSettings=(K=V,K="V",...)
func parseOptionSettings(content string) (map[string]string, error) {
	m := optionLineRe.FindStringSubmatch(content)
	if m == nil {
		return nil, fmt.Errorf("未找到 OptionSettings=(...) 配置行")
	}
	return splitPairs(m[1]), nil
}

func splitPairs(body string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(body, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 {
			out[strings.TrimSpace(kv[0])] = unquoteOptionValue(kv[1])
		}
	}
	return out
}

func unquoteOptionValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}

// 布尔/数值保持无引号，其余按字符串加引号（用于文件中尚不存在的新键）
var optionBareValueRe = regexp.MustCompile(`^(True|False|-?[0-9]+(\.[0-9]+)?)$`)

// sanitizeOptionValue 移除会破坏 OptionSettings 单行格式的字符
func sanitizeOptionValue(v string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '"', ',', '(', ')', '\n', '\r':
			return -1
		}
		return r
	}, v)
}

// substituteOptionValues 在原始内容上就地替换键值：保留注释、CRLF、键顺序和引号风格
func substituteOptionValues(content string, values map[string]string) (string, error) {
	m := optionLineRe.FindStringSubmatchIndex(content)
	if m == nil {
		return "", fmt.Errorf("未找到 OptionSettings=(...) 配置行")
	}
	body := content[m[2]:m[3]]
	for k, v := range values {
		v = sanitizeOptionValue(v)
		// 用 ( 或 , 锚定精确键名，捕获旧值以判断引号风格
		re := regexp.MustCompile(`([(,]\s*` + regexp.QuoteMeta(k) + `\s*=\s*)("[^",)]*"|[^,)]*)`)
		idx := re.FindStringSubmatchIndex(body)
		if idx == nil {
			// 键不存在：追加到末尾
			pair := k + `="` + v + `"`
			if optionBareValueRe.MatchString(v) {
				pair = k + "=" + v
			}
			if body != "" {
				body += ","
			}
			body += pair
			continue
		}
		old := body[idx[4]:idx[5]]
		repl := v
		if strings.HasPrefix(old, `"`) {
			repl = `"` + v + `"`
		}
		body = body[:idx[4]] + repl + body[idx[5]:]
	}
	return content[:m[2]] + body + content[m[3]:], nil
}

// parseKV 解析简单 Key=Value 行（保留原行序信息由调用方处理）
func parseKV(content string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) == 2 {
			out[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return out
}

// readConfigFile 按 spec 读取配置：返回结构化字段值或原文
func readConfigFile(instDir string, spec *ConfigSpec) (map[string]string, string, error) {
	path := instDir + "/" + spec.Path
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	content := string(data)
	switch spec.Format {
	case "option-settings":
		kv, err := parseOptionSettings(content)
		return kv, content, err
	case "kv":
		return parseKV(content), content, nil
	default:
		return nil, content, nil
	}
}

// writeConfigFile 按 spec 写回：option-settings 保留原键序；raw 整体替换
func writeConfigFile(instDir string, spec *ConfigSpec, values map[string]string, raw *string) error {
	path := instDir + "/" + spec.Path
	if spec.Format == "raw" {
		if raw == nil {
			return fmt.Errorf("raw 格式需要完整内容")
		}
		if err := os.WriteFile(path, []byte(*raw), 0644); err != nil {
			return err
		}
		return chownToGames(path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	content := string(data)

	switch spec.Format {
	case "option-settings":
		out, err := substituteOptionValues(content, values)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(out), 0644); err != nil {
			return err
		}
		return chownToGames(path)
	case "kv":
		lines := strings.Split(content, "\n")
		written := map[string]bool{}
		for i, line := range lines {
			kv := strings.SplitN(line, "=", 2)
			if len(kv) != 2 {
				continue
			}
			k := strings.TrimSpace(kv[0])
			if v, ok := values[k]; ok {
				lines[i] = k + "=" + v
				written[k] = true
			}
		}
		for k, v := range values {
			if !written[k] {
				lines = append(lines, k+"="+v)
			}
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644); err != nil {
			return err
		}
		return chownToGames(path)
	}
	return fmt.Errorf("未知格式 %q", spec.Format)
}

// seedConfigFile 安装后从 seed 生成目标配置（若目标不存在）
func seedConfigFile(instDir string, spec *ConfigSpec) error {
	if spec.SeedFrom == "" {
		return nil
	}
	target := instDir + "/" + spec.Path
	if _, err := os.Stat(target); err == nil {
		return nil // 已存在，不覆盖
	}
	data, err := os.ReadFile(instDir + "/" + spec.SeedFrom)
	if err != nil {
		return fmt.Errorf("读取种子配置 %s: %w", spec.SeedFrom, err)
	}
	if err := mkdirForGames(path2dir(target)); err != nil {
		return err
	}
	if err := os.WriteFile(target, data, 0644); err != nil {
		return err
	}
	return chownToGames(target)
}

func path2dir(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "."
	}
	return p[:i]
}
