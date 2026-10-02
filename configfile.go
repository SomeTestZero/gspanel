package main

import (
	"fmt"
	"html"
	"os"
	"regexp"
	"strings"
)

// ---------- 游戏配置文件读写：option-settings（Palworld）/ kv / xmlkv / raw ----------
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
		// 用 ( 或 , 或 body 起始锚定精确键名（首键前面没有定界符），捕获旧值以判断引号风格
		re := regexp.MustCompile(`(^|[(,])\s*` + regexp.QuoteMeta(k) + `\s*=\s*("[^",)]*"|[^,)]*)`)
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

// ---------- xmlkv：XML 属性式配置（七日杀 serverconfig.xml 风格）----------
//
// <property name="X"  value="Y"/> 逐行排布 + 行尾注释。写入时只替换 value 属性，
// 保留注释/空行/排版；文件中不存在的键插入到根元素闭合标签前。

var xmlPropLineRe = regexp.MustCompile(`<property\s+name="([^"]+)"\s+value="([^"]*)"`)
var xmlClosingTagRe = regexp.MustCompile(`(?m)^</[A-Za-z0-9_:.-]+>\s*$`)

// parseXMLKV 解析所有 <property name="K" value="V"/> 行（实体自动反转义）
func parseXMLKV(content string) map[string]string {
	out := map[string]string{}
	for _, m := range xmlPropLineRe.FindAllStringSubmatch(content, -1) {
		out[html.UnescapeString(m[1])] = html.UnescapeString(m[2])
	}
	return out
}

// xmlEscapeAttrValue 转义属性值并去掉换行（属性值不能跨行）
func xmlEscapeAttrValue(v string) string {
	v = strings.ReplaceAll(v, "\r", "")
	v = strings.ReplaceAll(v, "\n", " ")
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(v)
}

// substituteXMLValues 就地替换 value 属性；缺的键插到根闭合标签前
func substituteXMLValues(content string, values map[string]string) string {
	for _, k := range sortedKeys(values) {
		v := values[k]
		re := regexp.MustCompile(`(<property\s+name="` + regexp.QuoteMeta(k) + `"\s+value=")[^"]*(")`)
		if loc := re.FindStringSubmatchIndex(content); loc != nil {
			// loc[3]:loc[4] 是旧值（组 1 结束到组 2 开始之间）
			content = content[:loc[3]] + xmlEscapeAttrValue(v) + content[loc[4]:]
			continue
		}
		line := "\t<property name=\"" + xmlEscapeAttrValue(k) + "\"\t\t\tvalue=\"" + xmlEscapeAttrValue(v) + "\"/>"
		if locs := xmlClosingTagRe.FindAllStringIndex(content, -1); len(locs) > 0 {
			m := locs[len(locs)-1]
			content = content[:m[0]] + line + "\n" + content[m[0]:]
		} else {
			content += "\n" + line + "\n"
		}
	}
	return content
}

// sortedKeys 保证缺失键插入顺序稳定（可重现输出）
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
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
	case "xmlkv":
		return parseXMLKV(content), content, nil
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
	case "xmlkv":
		out := substituteXMLValues(content, values)
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
