package main

import (
	"encoding/json"
	"testing"
)

func testTable(t *testing.T) *SandboxTable {
	t.Helper()
	tb, err := sandboxTable("7dtd-v3.2")
	if err != nil {
		t.Fatalf("选项表未加载: %v", err)
	}
	return tb
}

// 选项表完整性：ID 连续唯一、base-26 编码与 ID 一致、默认值合法
func TestSandboxTableIntegrity(t *testing.T) {
	tb := testTable(t)
	if len(tb.Options) == 0 {
		t.Fatal("选项表为空")
	}
	for i, o := range tb.Options {
		if o.ID != i {
			t.Errorf("选项 %s ID=%d，应为 %d（枚举声明序）", o.Name, o.ID, i)
		}
		if len(o.Values) == 0 {
			t.Errorf("选项 %s 没有取值列表", o.Name)
		}
		if o.Default < 0 || o.Default >= len(o.Values) {
			t.Errorf("选项 %s 默认值索引 %d 越界", o.Name, o.Default)
		}
		if len(o.Values) > 26 {
			t.Errorf("选项 %s 取值 %d 个超出编码上限 26", o.Name, len(o.Values))
		}
		if o.Label == "" {
			t.Errorf("选项 %s 缺中文名", o.Name)
		}
	}
	// 与游戏 gso 实测一致的关键项（V3.2.0）
	for name, want := range map[string]int{"DropOnDeath": 1, "DeathPenalty": 1, "AISmellMode": 3, "IncomingDamage": 7} {
		o := tb.byName[name]
		if o == nil {
			t.Fatalf("缺选项 %s", name)
		}
		if o.Default != want {
			t.Errorf("%s 默认值索引=%d，实测 gso 应为 %d", name, o.Default, want)
		}
	}
}

// 已知预设代码解码（Adventurer：4 个伤害倍率 + IncomingDamage + AISmellMode 共 6 项）
func TestSandboxDecodeKnown(t *testing.T) {
	tb := testTable(t)
	vals, err := decodeSandboxCode(tb, "AAAJABJACJADJARFBNC")
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]int{0: 9, 1: 9, 2: 9, 3: 9, 17: 5, 39: 2}
	if len(vals) != len(want) {
		t.Fatalf("解出 %d 项，应为 %d 项：%v", len(vals), len(want), vals)
	}
	for id, idx := range want {
		if vals[id] != idx {
			t.Errorf("选项 %d = %d，应为 %d", id, vals[id], idx)
		}
	}
	if v, err := decodeSandboxCode(tb, "A"); err != nil || len(v) != 0 {
		t.Errorf("代码 A 应为全默认，got %v err=%v", v, err)
	}
	// 编码：已知取值 -> 已知代码；默认值项省略（与游戏生成规则一致）
	if code, err := encodeSandboxCode(tb, want); err != nil || code != "AAAJABJACJADJARFBNC" {
		t.Errorf("编码不一致: %q err=%v", code, err)
	}
	if code, err := encodeSandboxCode(tb, map[int]int{39: 3}); err != nil || code != "A" {
		t.Errorf("默认值项应省略: %q err=%v", code, err)
	}
}

// 全部官方预设代码：解码合法 + 重新编码后语义逐项一致。
// 不要求字符串相等：部分预设含「等于 V3.2 默认值」的块（V3.0 旧默认残留），
// 规范编码会省略它们（与游戏一致：全默认 = "A"），语义不变。
func TestSandboxPresetRoundTrip(t *testing.T) {
	tb := testTable(t)
	effective := func(vals map[int]int) map[int]int {
		out := make(map[int]int, len(tb.Options))
		for _, o := range tb.Options {
			out[o.ID] = o.Default
		}
		for id, idx := range vals {
			out[id] = idx
		}
		return out
	}
	raw, err := embedded.ReadFile("templates/7dtd.json")
	if err != nil {
		t.Fatal(err)
	}
	var tmpl struct {
		Configs []struct {
			Schema []struct {
				Key     string `json:"key"`
				Presets []struct {
					Value string `json:"value"`
				} `json:"presets"`
			} `json:"schema"`
		} `json:"configs"`
	}
	if err := json.Unmarshal(raw, &tmpl); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, cfg := range tmpl.Configs {
		for _, f := range cfg.Schema {
			if f.Key != "SandboxCode" {
				continue
			}
			for _, p := range f.Presets {
				n++
				vals, err := decodeSandboxCode(tb, p.Value)
				if err != nil {
					t.Errorf("预设 %q 解码失败: %v", p.Value, err)
					continue
				}
				code, err := encodeSandboxCode(tb, vals)
				if err != nil {
					t.Errorf("预设 %q 编码失败: %v", p.Value, err)
					continue
				}
				vals2, err := decodeSandboxCode(tb, code)
				if err != nil {
					t.Errorf("预设 %q 重编码后解码失败: %v (%s)", p.Value, err, code)
					continue
				}
				if a, b := effective(vals), effective(vals2); !mapsEqual(a, b) {
					t.Errorf("预设 %q 往返语义不一致 -> %q", p.Value, code)
				}
			}
		}
	}
	if n < 17 {
		t.Errorf("预设只有 %d 个，应有 17 个", n)
	}
}

func TestSandboxCodecErrors(t *testing.T) {
	tb := testTable(t)
	for _, code := range []string{"", "B", "AB", "AAaA", "ZZZ", "AGJA", "AAJAAJ"} {
		if _, err := decodeSandboxCode(tb, code); err == nil {
			t.Errorf("非法代码 %q 应报错", code)
		}
	}
	// 越界值索引
	if _, err := encodeSandboxCode(tb, map[int]int{27: 99}); err == nil {
		t.Error("越界值索引应报错")
	}
	if _, err := encodeSandboxCode(tb, map[int]int{999: 0}); err == nil {
		t.Error("未知选项 ID 应报错")
	}
	// 名称键转换
	vals, err := tb.idValues(map[string]int{"DropOnDeath": 0})
	if err != nil || vals[27] != 0 {
		t.Errorf("idValues 失败: %v %v", vals, err)
	}
	if _, err := tb.idValues(map[string]int{"NoSuchOption": 0}); err == nil {
		t.Error("未知选项名应报错")
	}
}

func mapsEqual(a, b map[int]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestParseGso(t *testing.T) {
	out := `2026-09-30T21:04:44 INF Executing command 'gso true' by Telnet
Sandbox Code: A
Sandbox Options:
*** GENERAL ***
Option RangedDamage: 7/100% (default: 7/100%)
Option DropOnDeath: 1/All (default: 1/All)
Option DeathPenalty: 2/Injured (default: 1/XP Only)
`
	code, cur, def := parseGso(out)
	if code != "A" {
		t.Errorf("code=%q", code)
	}
	if cur["DeathPenalty"] != 2 || def["DeathPenalty"] != 1 || cur["DropOnDeath"] != 1 {
		t.Errorf("解析错误: cur=%v def=%v", cur, def)
	}
	if len(cur) != 3 {
		t.Errorf("应解析 3 个选项，got %d", len(cur))
	}
}
