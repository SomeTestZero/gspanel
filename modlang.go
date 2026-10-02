package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ---------- Mod 中文翻译（缓存 + 内置词典 + 可选 LLM + 免费接口兜底）----------
//
// 翻译链：内存/磁盘缓存 data/modlang.json -> 内置词典（游戏自带组件等，离线可用）
//        -> OpenAI 兼容接口（设置页可配，质量最好）-> MyMemory 免费接口（无需 key）。
// 结果按「源文本」缓存，跨 mod 复用；失败只降级不报错（界面继续显示英文）。

var builtinModZh = map[string]string{
	// 游戏自带的 TFP 官方组件（七日杀专用服务器的 Mods/ 里默认就有）
	"Harmony Wrapper": "Harmony 补丁框架封装",
	"TFP_Harmony":     "Harmony 补丁框架封装",
	"Sets up basic HarmonyX configuration to be used with 7 Days to Die": "为七日杀初始化 HarmonyX 补丁框架基础环境（游戏自带，其他官方组件的运行依赖）",
	"Server Command Extensions":                    "服务器命令扩展",
	"TFP_CommandExtensions":                        "服务器命令扩展",
	"Additional commands for server operation":     "为服务器运维提供额外的管理命令",
	"Markers (Example Web Mod)":                    "地图标记（Web 示例 Mod）",
	"Allows placing custom markers on the web map": "允许在网页地图上放置自定义标记（配合 Web 面板的地图渲染使用）",
	// 常见 ModInfo 词汇兜底
	"Unknown":        "未知",
	"No description": "无描述",
	// Nexus 七日杀分类（v1 games.json 的固定分类表）
	"7 Days To Die":        "综合",
	"Miscellaneous":        "杂项",
	"XML Edits":            "XML 修改",
	"User Interface":       "界面 UI",
	"Overhauls":            "大型改造",
	"Gameplay":             "玩法",
	"Prefabs":              "预制建筑 POI",
	"Items and Loot":       "物品与战利品",
	"Utilities":            "实用工具",
	"Crafting":             "制造",
	"Zombies":              "僵尸",
	"Vehicles":             "载具",
	"Cheats":               "作弊",
	"Visuals and Graphics": "画面视觉",
	"Quests":               "任务",
	"Weapons":              "武器",
	"Items - Food":         "食物物品",
	"Creatures":            "生物",
	"Audio":                "音效",
	"Maps":                 "地图",
	"Environment":          "环境",
	"Avatars":              "角色外观",
	// 常见标签
	"UI":                "界面",
	"Quality of Life":   "体验优化",
	"Visual":            "画面",
	"Items":             "物品",
	"Bug Fixes":         "问题修复",
	"All-In-One":        "整合包",
	"Cheating":          "作弊",
	"English":           "英语",
	"NPC Vendors":       "NPC 商人",
	"Animation":         "动画",
	"Replacer":          "替换类",
	"Lore-friendly":     "贴合原作",
	"Fair and Balanced": "平衡性",
	"Advanced Setup":    "进阶设置",
	"DLC Required":      "需要 DLC",
	"Magic":             "魔法类",
	"Adult":             "成人内容",
	"Real World Issues": "现实议题",
}

var (
	modLangMu     sync.RWMutex
	modLangMem    map[string]string
	modLangLoaded bool
)

func modLangPath() string { return DataDir + "/modlang.json" }

func modLangLoad() map[string]string {
	modLangMu.RLock()
	if modLangLoaded {
		defer modLangMu.RUnlock()
		return modLangMem
	}
	modLangMu.RUnlock()
	modLangMu.Lock()
	defer modLangMu.Unlock()
	if modLangLoaded {
		return modLangMem
	}
	m := map[string]string{}
	if data, err := os.ReadFile(modLangPath()); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	modLangMem, modLangLoaded = m, true
	return m
}

func modLangSaveLocked() error {
	data, err := json.MarshalIndent(modLangMem, "", "  ")
	if err != nil {
		return err
	}
	tmp := modLangPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, modLangPath())
}

// modLangLookup 查译文：缓存 -> 内置词典
func modLangLookup(text string) string {
	if text == "" {
		return ""
	}
	if zh := modLangLoad()[text]; zh != "" {
		return zh
	}
	return builtinModZh[text]
}

func modLangStore(text, zh string) {
	if text == "" || zh == "" || text == zh {
		return
	}
	modLangLoad() // 先确保已加载（不能持锁调用）
	modLangMu.Lock()
	defer modLangMu.Unlock()
	if modLangMem[text] == zh {
		return
	}
	modLangMem[text] = zh
	_ = modLangSaveLocked()
}

// ---------- 翻译 provider ----------

// parseOpenAIContent 从 LLM 响应里解析 {"0":"译文",...}（容忍 ```json 包裹）
func parseOpenAIContent(content string) map[string]string {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)
	out := map[string]string{}
	if err := json.Unmarshal([]byte(content), &out); err == nil {
		return out
	}
	// 兜底：抓第一个 {...}
	if m := regexp.MustCompile(`\{[^{}]*\}`).FindString(content); m != "" {
		_ = json.Unmarshal([]byte(m), &out)
	}
	return out
}

// translateOpenAI 走 OpenAI 兼容 chat/completions 批量翻译
func translateOpenAI(base, key, model string, texts []string) (map[string]string, error) {
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": "你是游戏服务器 Mod 的翻译器。输入是 JSON 数组（英文短文本），" +
				"输出 JSON 对象：键为元素下标的字符串形式，值为简体中文译文。只输出 JSON，不要解释。" +
				"术语参考：mod=Mod，server=服务器，loot=战利品，zombie=僵尸，blood moon=血月，trader=商人，quest=任务，crafting=制造，horde=尸潮。"},
			{"role": "user", "content": string(mustJSON(texts))},
		},
		"temperature": 0,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+"/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("翻译接口请求失败: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("翻译接口 HTTP %d: %s", resp.StatusCode, truncate(string(data), 160))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil || len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("翻译接口响应解析失败")
	}
	idxMap := parseOpenAIContent(parsed.Choices[0].Message.Content)
	out := map[string]string{}
	for k, v := range idxMap {
		var i int
		if _, err := fmt.Sscanf(k, "%d", &i); err == nil && i >= 0 && i < len(texts) && v != "" {
			out[texts[i]] = v
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("翻译接口未返回有效译文")
	}
	return out, nil
}

// parseMyMemory 从 MyMemory 响应取译文
func parseMyMemory(data []byte) (string, error) {
	var parsed struct {
		ResponseData struct {
			TranslatedText string `json:"translatedText"`
		} `json:"responseData"`
		ResponseStatus int    `json:"responseStatus"`
		Details        string `json:"responseDetails"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("翻译响应解析失败")
	}
	if parsed.ResponseStatus != 200 || parsed.ResponseData.TranslatedText == "" {
		return "", fmt.Errorf("免费翻译接口返回异常: %s", truncate(parsed.Details, 120))
	}
	return html.UnescapeString(parsed.ResponseData.TranslatedText), nil
}

// translateMyMemory 免费接口逐条翻译（无需 key；文本截断到 480 字符以内）
func translateMyMemory(text string) (string, error) {
	q := text
	if len(q) > 480 {
		q = q[:480]
		if i := strings.LastIndexAny(q, " .,;"); i > 100 {
			q = q[:i]
		}
	}
	u := "https://api.mymemory.translated.net/get?q=" + url.QueryEscape(q) + "&langpair=en|zh-CN"
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Get(u)
	if err != nil {
		return "", fmt.Errorf("免费翻译接口不可用: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return parseMyMemory(data)
}

// translateTexts 批量翻译缺失的文本并入缓存，返回新翻译条数
func (sv *Server) translateTexts(texts []string) (int, error) {
	need := []string{}
	seen := map[string]bool{}
	for _, t := range texts {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] || modLangLookup(t) != "" {
			continue
		}
		seen[t] = true
		need = append(need, t)
	}
	if len(need) == 0 {
		return 0, nil
	}

	sv.state.mu.RLock()
	base, key, model := sv.state.TranslateBaseURL, sv.state.TranslateAPIKey, sv.state.TranslateModel
	sv.state.mu.RUnlock()

	count := 0
	var lastErr error
	if base != "" && key != "" {
		got, err := translateOpenAI(base, key, model, need)
		if err != nil {
			lastErr = err
		} else {
			for src, zh := range got {
				modLangStore(src, zh)
			}
			count += len(got)
			rest := []string{}
			for _, t := range need {
				if modLangLookup(t) == "" {
					rest = append(rest, t)
				}
			}
			need = rest
		}
	}
	for _, t := range need {
		zh, err := translateMyMemory(t)
		if err != nil {
			lastErr = err
			continue
		}
		modLangStore(t, zh)
		count++
	}
	if count == 0 && lastErr != nil {
		return 0, lastErr
	}
	return count, nil
}

func mustJSON(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

// ---------- HTTP handler ----------

// handleTranslateText 通用翻译（Mod 商店浏览等用）：body {texts:[...]} -> {translations:{原文:译文}}
// 已有译文的文本不重复翻译；整体失败时返回已缓存部分，不报错打断浏览
func (sv *Server) handleTranslateText(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Texts []string `json:"texts"`
	}
	if !decodeJSON(w, r, &req) || len(req.Texts) == 0 {
		jsonError(w, http.StatusBadRequest, "texts 不能为空")
		return
	}
	if len(req.Texts) > 80 {
		req.Texts = req.Texts[:80]
	}
	_, _ = sv.translateTexts(req.Texts)
	out := map[string]string{}
	for _, t := range req.Texts {
		if zh := modLangLookup(t); zh != "" {
			out[t] = zh
		}
	}
	jsonOK(w, map[string]any{"translations": out})
}

// handleSetTranslate 保存 Mod 翻译服务配置（OpenAI 兼容接口；清空 base_url = 恢复默认免费链路）
func (sv *Server) handleSetTranslate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
		Model   string `json:"model"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	sv.state.mu.Lock()
	if strings.TrimSpace(req.BaseURL) == "" {
		sv.state.TranslateBaseURL, sv.state.TranslateAPIKey, sv.state.TranslateModel = "", "", ""
	} else {
		sv.state.TranslateBaseURL = strings.TrimSpace(req.BaseURL)
		sv.state.TranslateModel = strings.TrimSpace(req.Model)
		if k := strings.TrimSpace(req.APIKey); k != "" { // key 留空 = 不修改
			sv.state.TranslateAPIKey = k
		}
	}
	configured := sv.state.TranslateBaseURL != ""
	err := sv.state.saveLocked()
	sv.state.mu.Unlock()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true, "configured": configured})
}

// handleModTranslate 翻译该实例 mod 列表的名称/描述（幂等，缓存命中不调接口）
func (sv *Server) handleModTranslate(w http.ResponseWriter, r *http.Request) {
	inst, tmpl, ok := sv.modMgrOf(w, r)
	if !ok {
		return
	}
	active, disabled := modDirs(inst, tmpl)
	mods := listModsAt(active, disabled)
	texts := []string{}
	for _, m := range mods {
		texts = append(texts, m.Name, m.Description)
	}
	n, err := sv.translateTexts(texts)
	if err != nil {
		jsonError(w, http.StatusBadGateway, fmt.Sprintf("翻译失败（已译 %d 条）: %v", n, err))
		return
	}
	enrichModZh(mods)
	jsonOK(w, map[string]any{"ok": true, "translated": n, "mods": mods})
}
