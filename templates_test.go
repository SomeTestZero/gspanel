package main

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// sandboxCodeRe 七日杀 SandboxCode：版本头 'A' + 若干 3 字符块（2 字符 base-26 选项 ID + 1 字符值索引）
var sandboxCodeRe = regexp.MustCompile(`^A([A-Z]{3})*$`)

// TestTemplatePresets 模板里所有预设按钮的基础校验 + 七日杀沙盒代码结构校验
func TestTemplatePresets(t *testing.T) {
	entries, err := os.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile("templates/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		var tpl GameTemplate
		if err := json.Unmarshal(data, &tpl); err != nil {
			t.Fatalf("%s 解析失败: %v", e.Name(), err)
		}
		for _, c := range tpl.Configs {
			for _, f := range c.Schema {
				for _, p := range f.Presets {
					if p.Label == "" || p.Value == "" {
						t.Errorf("%s: %s 存在缺 label/value 的预设", e.Name(), f.Key)
					}
				}
				if tpl.ID != "7dtd" || f.Key != "SandboxCode" {
					continue
				}
				if f.DecodeCommand != "gso" {
					t.Errorf("7dtd SandboxCode 应声明 decode_command=gso")
				}
				if len(f.Presets) < 17 {
					t.Errorf("7dtd SandboxCode 预设应为 17 个，实际 %d", len(f.Presets))
				}
				for _, p := range f.Presets {
					if !sandboxCodeRe.MatchString(p.Value) {
						t.Errorf("预设 %q 代码结构非法: %q", p.Label, p.Value)
						continue
					}
					body := p.Value[1:]
					for i := 0; i < len(body); i += 3 {
						id := int(body[i]-'A')*26 + int(body[i+1]-'A')
						if id >= 166 { // V3.2 SandboxOptions 枚举 166 项（含 Max 哨兵）
							t.Errorf("预设 %q 含越界选项 id %d", p.Label, id)
						}
					}
				}
			}
		}
	}
}
