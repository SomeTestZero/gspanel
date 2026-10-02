package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuiltinModZh(t *testing.T) {
	if got := modLangLookup("Harmony Wrapper"); got != "Harmony 补丁框架封装" {
		t.Fatalf("内置词典未命中: %q", got)
	}
	if got := modLangLookup("不存在的文本"); got != "" {
		t.Fatalf("未知文本应返回空: %q", got)
	}
}

func TestModLangCacheRoundTrip(t *testing.T) {
	// 隔离缓存文件
	DataDir = t.TempDir()
	modLangMu.Lock()
	modLangMem, modLangLoaded = map[string]string{}, true
	modLangMu.Unlock()

	modLangStore("Some Mod Name", "某个模组名")
	if got := modLangLookup("Some Mod Name"); got != "某个模组名" {
		t.Fatalf("缓存未命中: %q", got)
	}
	if _, err := os.Stat(filepath.Join(DataDir, "modlang.json")); err != nil {
		t.Fatalf("缓存文件未落盘: %v", err)
	}
	// 模拟重启（重新从磁盘加载）
	modLangMu.Lock()
	modLangMem, modLangLoaded = nil, false
	modLangMu.Unlock()
	if got := modLangLookup("Some Mod Name"); got != "某个模组名" {
		t.Fatalf("磁盘缓存未命中: %q", got)
	}
}

func TestParseOpenAIContent(t *testing.T) {
	got := parseOpenAIContent("```json\n{\"0\": \"译文一\", \"1\": \"译文二\"}\n```")
	if got["0"] != "译文一" || got["1"] != "译文二" {
		t.Fatalf("markdown 包裹解析失败: %+v", got)
	}
	got = parseOpenAIContent(`说明：{"0":"直接JSON"} 完`)
	if got["0"] != "直接JSON" {
		t.Fatalf("内嵌 JSON 兜底解析失败: %+v", got)
	}
}

func TestParseMyMemory(t *testing.T) {
	zh, err := parseMyMemory([]byte(`{"responseData":{"translatedText":"Harmony Wrapper&#39;s setup"},"responseStatus":200}`))
	if err != nil || zh != "Harmony Wrapper's setup" {
		t.Fatalf("解析失败: %q %v", zh, err)
	}
	if _, err := parseMyMemory([]byte(`{"responseData":{"translatedText":""},"responseStatus":429,"responseDetails":"MYMEMORY WARNING: USAGE LIMIT REACHED"}`)); err == nil {
		t.Fatal("配额耗尽应报错")
	}
	if _, err := parseMyMemory([]byte(`not json`)); err == nil {
		t.Fatal("非法响应应报错")
	}
}

func TestModSideHint(t *testing.T) {
	cases := []struct {
		name, summary string
		tags          []string
		want          string
	}{
		{"Server Side Loot Expansion", "Install on the server only", nil, "server"},
		{"HD Textures", "client-side visual mod", nil, "client"},
		{"Big Mod", "install on both the client and server", nil, "both"},
		{"SMX UI Pack", "", []string{"UI"}, "client?"},
		{"Mystery", "does stuff", nil, ""},
	}
	for _, c := range cases {
		if got := modSideHint(c.name, c.summary, c.tags); got != c.want {
			t.Errorf("modSideHint(%q,%q,%v) = %q, want %q", c.name, c.summary, c.tags, got, c.want)
		}
	}
}

func TestGameVersionHint(t *testing.T) {
	if got := gameVersionHint("MyMod V3.2 Update", "3.2.0"); got != "match:3.2" {
		t.Fatalf("同版本应 match: %q", got)
	}
	if got := gameVersionHint("Bigger Backpack (A21)", "3.2.0"); got != "maybe:A21" {
		t.Fatalf("旧 Alpha 标记应 maybe:A21: %q", got)
	}
	if got := gameVersionHint("something v2.6", "3.2.0"); got != "maybe:2.6" {
		t.Fatalf("标注了别的版本应 maybe:2.6: %q", got)
	}
	if got := gameVersionHint("mod version 1.0.2 released", "3.2.0"); got != "" {
		t.Fatalf("裸数字版本不应误报: %q", got)
	}
	if got := gameVersionHint("no version here", ""); got != "" {
		t.Fatalf("无服务端版本应返回空: %q", got)
	}
	if got := gameVersionHint("no version here", "3.2.0"); got != "" {
		t.Fatalf("无版本信息应返回空: %q", got)
	}
}
