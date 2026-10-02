package main

// 七日杀沙盒选项（SandboxCode）可视化编辑器后端。
//
// SandboxCode 是 V3.0 起游戏把全部玩法规则（难度/死亡掉落/血月/战利品等 150+ 项）压缩成的
// 一串代码：'A' + 若干 3 字符块（2 字符 base-26 选项 ID + 1 字符值索引 'A'=0），默认值项省略。
// 选项 ID = SandboxOptions 枚举声明序。本文件做代码<->逐项取值的编解码；
// 选项表（名称/中文文案/取值列表/默认值）由 tools/7dtd-sandbox/extract.py 从游戏本体提取，
// 产物 assets/7dtd-sandbox/options.json 嵌入二进制。游戏更新后重跑提取脚本即可。

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed assets/7dtd-sandbox
var sandboxAssets embed.FS

type SandboxValue struct {
	Raw   any    `json:"raw"`
	Label string `json:"label"`
}

type SandboxOption struct {
	ID       int            `json:"id"`
	Name     string         `json:"name"`
	Label    string         `json:"label"`
	Desc     string         `json:"desc,omitempty"`
	Category string         `json:"category"`
	Values   []SandboxValue `json:"values"`
	Default  int            `json:"default"`
}

type SandboxCategory struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

type SandboxTable struct {
	ID          string            `json:"id"`
	Source      string            `json:"source"`
	OptionCount int               `json:"option_count"`
	Categories  []SandboxCategory `json:"categories"`
	Options     []SandboxOption   `json:"options"`

	byName map[string]*SandboxOption
	byID   map[int]*SandboxOption
}

func (t *SandboxTable) index() {
	t.byName = make(map[string]*SandboxOption, len(t.Options))
	t.byID = make(map[int]*SandboxOption, len(t.Options))
	for i := range t.Options {
		t.byName[t.Options[i].Name] = &t.Options[i]
		t.byID[t.Options[i].ID] = &t.Options[i]
	}
}

var sandboxTables = loadSandboxTables()

func loadSandboxTables() map[string]*SandboxTable {
	tables := map[string]*SandboxTable{}
	entries, err := sandboxAssets.ReadDir("assets/7dtd-sandbox")
	if err != nil {
		return tables
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := sandboxAssets.ReadFile("assets/7dtd-sandbox/" + e.Name())
		if err != nil {
			continue
		}
		var t SandboxTable
		if json.Unmarshal(raw, &t) != nil || t.ID == "" {
			continue
		}
		t.index()
		tables[t.ID] = &t
	}
	return tables
}

func sandboxTable(id string) (*SandboxTable, error) {
	t, ok := sandboxTables[id]
	if !ok {
		return nil, fmt.Errorf("沙盒选项表 %q 不存在", id)
	}
	return t, nil
}

// decodeSandboxCode 解析沙盒代码 -> 选项ID->值索引（只含代码里出现的项，默认项被省略）
func decodeSandboxCode(t *SandboxTable, code string) (map[int]int, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, errors.New("沙盒代码为空")
	}
	if code[0] != 'A' {
		return nil, errors.New("沙盒代码应以 A 开头")
	}
	if (len(code)-1)%3 != 0 {
		return nil, fmt.Errorf("沙盒代码长度不对（应为 1+3n 个字符）：%d", len(code))
	}
	values := map[int]int{}
	for i := 1; i < len(code); i += 3 {
		c0, c1, c2 := code[i], code[i+1], code[i+2]
		if c0 < 'A' || c0 > 'Z' || c1 < 'A' || c1 > 'Z' || c2 < 'A' || c2 > 'Z' {
			return nil, fmt.Errorf("沙盒代码第 %d 块含非法字符 %q", (i-1)/3+1, code[i:i+3])
		}
		id := int(c0-'A')*26 + int(c1-'A')
		idx := int(c2 - 'A')
		opt, ok := t.byID[id]
		if !ok {
			return nil, fmt.Errorf("选项 ID %d 不存在（代码可能来自其他游戏版本）", id)
		}
		if idx < 0 || idx >= len(opt.Values) {
			return nil, fmt.Errorf("选项 %s 的值索引 %d 超出范围 0~%d", opt.Name, idx, len(opt.Values)-1)
		}
		if _, dup := values[id]; dup {
			return nil, fmt.Errorf("选项 %s 在代码中重复出现", opt.Name)
		}
		values[id] = idx
	}
	return values, nil
}

// encodeSandboxCode 逐项取值 -> 沙盒代码（只编码非默认项，按选项 ID 升序，与游戏生成一致）
func encodeSandboxCode(t *SandboxTable, values map[int]int) (string, error) {
	ids := make([]int, 0, len(values))
	for id, idx := range values {
		opt, ok := t.byID[id]
		if !ok {
			return "", fmt.Errorf("选项 ID %d 不存在", id)
		}
		if idx < 0 || idx >= len(opt.Values) {
			return "", fmt.Errorf("选项 %s 的值索引 %d 超出范围 0~%d", opt.Name, idx, len(opt.Values)-1)
		}
		if idx == opt.Default {
			continue
		}
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var b strings.Builder
	b.WriteByte('A')
	for _, id := range ids {
		b.WriteByte(byte('A' + id/26))
		b.WriteByte(byte('A' + id%26))
		b.WriteByte(byte('A' + values[id]))
	}
	return b.String(), nil
}

// 名称键 <-> ID 键的转换（HTTP 接口用名称，便于前端展示）
func (t *SandboxTable) namedValues(idVals map[int]int) map[string]int {
	out := make(map[string]int, len(idVals))
	for id, idx := range idVals {
		if opt := t.byID[id]; opt != nil {
			out[opt.Name] = idx
		}
	}
	return out
}

func (t *SandboxTable) idValues(named map[string]int) (map[int]int, error) {
	out := make(map[int]int, len(named))
	for name, idx := range named {
		opt, ok := t.byName[name]
		if !ok {
			return nil, fmt.Errorf("选项 %s 不存在", name)
		}
		if idx < 0 || idx >= len(opt.Values) {
			return nil, fmt.Errorf("选项 %s 的值索引 %d 超出范围 0~%d", name, idx, len(opt.Values)-1)
		}
		out[opt.ID] = idx
	}
	return out, nil
}

/* ---------------- HTTP ---------------- */

// GET /api/sandbox/tables/{id}：选项表（分类/选项/取值/默认值）
func (sv *Server) handleSandboxTable(w http.ResponseWriter, r *http.Request) {
	t, err := sandboxTable(r.PathValue("id"))
	if err != nil {
		jsonError(w, http.StatusNotFound, err.Error())
		return
	}
	jsonOK(w, t)
}

// POST /api/sandbox/decode {table, code} -> {values:{选项名:值索引}, changed:[选项名]}
func (sv *Server) handleSandboxDecode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Table string `json:"table"`
		Code  string `json:"code"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	t, err := sandboxTable(req.Table)
	if err != nil {
		jsonError(w, http.StatusNotFound, err.Error())
		return
	}
	idVals, err := decodeSandboxCode(t, req.Code)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	changed := []string{}
	for id, idx := range idVals {
		if opt := t.byID[id]; opt != nil && idx != opt.Default {
			changed = append(changed, opt.Name)
		}
	}
	sort.Strings(changed)
	jsonOK(w, map[string]any{"values": t.namedValues(idVals), "changed": changed})
}

// POST /api/sandbox/encode {table, values:{选项名:值索引}} -> {code}
func (sv *Server) handleSandboxEncode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Table  string         `json:"table"`
		Values map[string]int `json:"values"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	t, err := sandboxTable(req.Table)
	if err != nil {
		jsonError(w, http.StatusNotFound, err.Error())
		return
	}
	idVals, err := t.idValues(req.Values)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	code, err := encodeSandboxCode(t, idVals)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonOK(w, map[string]any{"code": code})
}

var (
	gsoCodeLine = regexp.MustCompile(`(?m)^Sandbox Code: (\S+)\s*$`)
	gsoOptLine  = regexp.MustCompile(`(?m)^Option (\w+): (\d+)/.*?\(default: (\d+)/`)
)

// parseGso 解析 gso（getsandboxoptions）输出 -> 当前代码 + 逐项当前/默认值索引
func parseGso(out string) (code string, cur, def map[string]int) {
	cur, def = map[string]int{}, map[string]int{}
	if m := gsoCodeLine.FindStringSubmatch(out); m != nil {
		code = m[1]
	}
	for _, m := range gsoOptLine.FindAllStringSubmatch(out, -1) {
		idx, _ := strconv.Atoi(m[2])
		didx, _ := strconv.Atoi(m[3])
		cur[m[1]] = idx
		def[m[1]] = didx
	}
	return
}

// GET /api/instances/{name}/sandbox/live：从运行中的服务器读取当前实际生效的沙盒选项（gso）
func (sv *Server) handleSandboxLive(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	tmpl := sv.templateOf(inst)
	if tmpl.RCON == nil || tmpl.RCON.Type != "telnet" {
		jsonError(w, http.StatusBadRequest, "该实例不支持在线读取沙盒选项（需要 telnet 控制台）")
		return
	}
	out, err := telnetExecConfig(inst, tmpl, "gso true")
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	code, cur, def := parseGso(out)
	var values map[string]int
	if t, err := sandboxTable(r.URL.Query().Get("table")); err == nil {
		// 以选项表为准返回；gso 未列出的项不返回（宁缺勿错）
		values = map[string]int{}
		for name, idx := range cur {
			if t.byName[name] != nil {
				values[name] = idx
			}
		}
	}
	jsonOK(w, map[string]any{"code": code, "values": values, "defaults": def, "raw": out})
}
