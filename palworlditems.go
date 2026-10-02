package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- Palworld 物品库（「给物品」下拉/搜索用） ----------
//
// 数据来源两级：
//  1. 内置基线 assets/palworld-items/palworld-zh.json（embed 进二进制，游戏没开也能用）
//  2. 实例运行时从游戏 DT_ItemDataTable 导出（mod 的 items 命令），存 data/items/palworld.json
//     优先级高于内置；游戏更新新增物品后点「从游戏刷新物品库」更新。
//
//go:embed assets/palworld-items/*.json
var palworldItemAssets embed.FS

const (
	palworldItemBaselineAsset = "assets/palworld-items/palworld-zh.json"
	palworldItemDBFile        = "items/palworld.json"
	itemRefreshTimeout        = 3 * time.Minute
)

// ItemEntry 单条物品；json 字段与 mod 导出的 items.json 一致
type ItemEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"`
	Type2    string `json:"type2,omitempty"`
	Rarity   int    `json:"rarity,omitempty"`
	MaxStack int    `json:"max_stack,omitempty"`
}

// ItemDB 持久化的物品库（data/items/palworld.json 与内置基线同格式）
type ItemDB struct {
	Source      string      `json:"source"`
	Culture     string      `json:"culture,omitempty"`
	GeneratedAt string      `json:"generated_at,omitempty"`
	GameBuild   string      `json:"game_build_id,omitempty"`
	GameVersion string      `json:"game_version,omitempty"`
	Count       int         `json:"count"`
	Items       []ItemEntry `json:"items"`

	// 以下字段只出现在 API 响应里，不落盘
	Builtin    bool   `json:"builtin"`
	Stale      bool   `json:"stale,omitempty"`
	ModVersion string `json:"mod_version,omitempty"`
}

// itemDump 是 mod items 命令写出的原始文件格式（1.0.4 只含物品 ID：
// 行结构体 PalStaticItemDataStruct 是精简表，没有 Name/TypeA 等字段）
type itemDump struct {
	OK         bool     `json:"ok"`
	ModVersion string   `json:"mod_version"`
	Path       string   `json:"path"`
	Count      int      `json:"count"`
	IDs        []string `json:"ids"`
}

func palworldItemDBPath() string { return filepath.Join(DataDir, palworldItemDBFile) }

func loadBundledItemDB() (*ItemDB, error) {
	data, err := palworldItemAssets.ReadFile(palworldItemBaselineAsset)
	if err != nil {
		return nil, err
	}
	var db ItemDB
	if err := json.Unmarshal(data, &db); err != nil {
		return nil, fmt.Errorf("解析内置物品库失败: %w", err)
	}
	db.Builtin = true
	return &db, nil
}

// loadItemDB 读取实例可用物品库：运行时导出优先，其次内置基线
func loadItemDB(inst *Instance) (*ItemDB, error) {
	if data, err := os.ReadFile(palworldItemDBPath()); err == nil && len(data) > 0 {
		var db ItemDB
		if err := json.Unmarshal(data, &db); err == nil && len(db.Items) > 0 {
			return &db, nil
		}
	}
	return loadBundledItemDB()
}

func saveItemDB(db *ItemDB) error {
	if err := os.MkdirAll(filepath.Dir(palworldItemDBPath()), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(db, "", " ")
	if err != nil {
		return err
	}
	tmp := palworldItemDBPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, palworldItemDBPath())
}

// itemCategoryLabels 把游戏枚举名翻译成中文分类；没有映射就原样显示
func itemCategoryLabels() map[string]string {
	return map[string]string{
		"None": "未分类", "Weapon": "武器", "WeaponMelee": "近战武器", "WeaponThrow": "投掷武器",
		"WeaponBow": "弓", "WeaponCrossbow": "弩", "WeaponGun": "枪械", "WeaponHandgun": "手枪",
		"WeaponRifle": "步枪", "WeaponShotgun": "霰弹枪", "WeaponSniper": "狙击枪", "WeaponRocket": "火箭筒",
		"WeaponGrenade": "手雷", "WeaponStaff": "法杖", "WeaponSword": "剑", "WeaponAxe": "斧",
		"WeaponPickaxe": "镐", "WeaponSpear": "长矛", "WeaponBat": "球棒", "WeaponClub": "棍棒",
		"Armor": "防具", "ArmorHead": "头部装备", "ArmorBody": "身体装备", "ArmorAccessory": "饰品",
		"Accessory": "饰品", "Shield": "盾牌", "Glider": "滑翔伞", "Ammo": "弹药", "Arrow": "箭",
		"Material": "材料", "MaterialWood": "木材", "MaterialStone": "石材", "MaterialMetal": "金属",
		"Consumable": "消耗品", "Food": "食物", "Ingredient": "食材", "Medicine": "药品",
		"Sphere": "帕鲁球", "PalSphere": "帕鲁球", "Blueprint": "图纸", "Tool": "工具",
		"Key": "钥匙", "KeyItem": "重要物品", "Essential": "重要物品", "Quest": "任务物品",
		"Currency": "货币", "Money": "货币", "Pal": "帕鲁", "PalEgg": "帕鲁蛋", "Egg": "帕鲁蛋",
		"Seed": "种子", "Crop": "作物", "Structure": "建筑", "Decor": "装饰", "Furniture": "家具",
		"Piece": "部件", "Oil": "石油", "Schematic": "图纸", "Monster": "怪物", "Animal": "动物",
		"Vehicle": "载具", "Skin": "皮肤", "Costume": "服装", "Hair": "发型", "Emote": "表情",
		"Attachment": "配件", "Module": "模块", "Upgrade": "强化材料", "Special": "特殊",
	}
}

// itemCategories 统计分类并翻译（按数量降序）
func itemCategories(items []ItemEntry) []map[string]any {
	labels := itemCategoryLabels()
	counts := map[string]int{}
	for _, it := range items {
		k := it.Type
		if k == "" {
			k = "None"
		}
		counts[k]++
	}
	out := make([]map[string]any, 0, len(counts))
	for k, n := range counts {
		label := labels[k]
		if label == "" {
			label = k
		}
		out = append(out, map[string]any{"key": k, "label": label, "count": n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i]["count"].(int) != out[j]["count"].(int) {
			return out[i]["count"].(int) > out[j]["count"].(int)
		}
		return out[i]["label"].(string) < out[j]["label"].(string)
	})
	return out
}

func itemMatches(it ItemEntry, q string) bool {
	if q == "" {
		return true
	}
	return strings.Contains(strings.ToLower(it.ID), q) ||
		strings.Contains(strings.ToLower(it.Name), q)
}

// ---------- HTTP ----------

// handleItemsList GET /api/instances/{name}/items?q=&type=&limit=
func (sv *Server) handleItemsList(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	tmpl := sv.templateOf(inst)
	if tmpl == nil || tmpl.ID != "palworld" {
		jsonError(w, http.StatusBadRequest, "该模板没有物品库")
		return
	}
	db, err := loadItemDB(inst)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	typ := r.URL.Query().Get("type")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items := db.Items
	if q != "" || typ != "" {
		filtered := make([]ItemEntry, 0, len(items))
		for _, it := range items {
			if typ != "" && it.Type != typ {
				continue
			}
			if itemMatches(it, q) {
				filtered = append(filtered, it)
			}
		}
		items = filtered
	}
	total := len(items)
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}

	// 运行时库是否落后于本地游戏 build（提示用户刷新）
	appID := tmpl.SteamAppID
	build := ""
	if id, err := localBuildID(inst.Dir, appID); err == nil {
		build = strconv.FormatInt(id, 10)
	}
	stale := !db.Builtin && build != "" && db.GameBuild != "" && db.GameBuild != build

	jsonOK(w, map[string]any{
		"source":         db.Source,
		"culture":        db.Culture,
		"generated_at":   db.GeneratedAt,
		"game_build_id":  db.GameBuild,
		"game_version":   db.GameVersion,
		"local_build_id": build,
		"builtin":        db.Builtin,
		"stale":          stale,
		"mod_version":    embeddedModVersion(),
		"total":          db.Count,
		"count":          total,
		"categories":     itemCategories(db.Items),
		"items":          items,
	})
}

// handleItemsRefresh POST /api/instances/{name}/items/refresh
// 让游戏里的 mod 从 DT_ItemDataTable 导出全量物品（含中文名），缓存到 data/items/
func (sv *Server) handleItemsRefresh(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	tmpl := sv.templateOf(inst)
	if tmpl == nil || tmpl.ID != "palworld" {
		jsonError(w, http.StatusBadRequest, "该模板没有物品库")
		return
	}
	if !hasGiveMod(inst) {
		jsonError(w, http.StatusBadRequest, "该实例未安装 gspanel 扩展命令 mod：请到「设置 → 扩展命令」先安装并重启实例")
		return
	}
	// 旧版 mod 不认识 items 命令：先握手拿版本，给出可操作的提示
	if ok, msg, err := sv.runModVerb(inst, "hello", nil, 10*time.Second); err != nil {
		jsonError(w, http.StatusBadGateway, "扩展命令无响应："+err.Error())
		return
	} else if !ok {
		jsonError(w, http.StatusConflict, "扩展命令返回失败（mod 可能是旧版）："+msg+"。请到「设置 → 扩展命令」重新安装并重启实例")
		return
	}
	ok, msg, err := sv.runModVerb(inst, "items", nil, itemRefreshTimeout)
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	if !ok {
		jsonError(w, http.StatusBadGateway, "物品导出失败："+msg)
		return
	}
	data, err := os.ReadFile(filepath.Join(modQueueDir(inst), "items.json"))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "读取 mod 导出的 items.json 失败: "+err.Error())
		return
	}
	var dump itemDump
	if err := json.Unmarshal(data, &dump); err != nil {
		jsonError(w, http.StatusInternalServerError, "解析 items.json 失败: "+err.Error())
		return
	}
	if len(dump.IDs) == 0 {
		jsonError(w, http.StatusBadGateway, "导出结果为空（游戏是否还在加载？）："+msg)
		return
	}
	// mod 只给物品 ID（游戏文本表的中文名由面板内置基线提供，按 ID 合并）
	baseline, _ := loadBundledItemDB()
	nameOf := map[string]string{}
	if baseline != nil {
		for _, it := range baseline.Items {
			nameOf[it.ID] = it.Name
		}
	}
	items := make([]ItemEntry, 0, len(dump.IDs))
	named := 0
	for _, id := range dump.IDs {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		name := nameOf[id]
		if name != "" {
			named++
		} else {
			name = id
		}
		items = append(items, ItemEntry{ID: id, Name: name})
	}
	build := ""
	if id, err := localBuildID(inst.Dir, tmpl.SteamAppID); err == nil {
		build = strconv.FormatInt(id, 10)
	}
	db := &ItemDB{
		Source:      "游戏导出（ID）+ 面板内置中文名",
		Culture:     "zh-Hans",
		GeneratedAt: time.Now().Format(time.RFC3339),
		GameBuild:   build,
		Count:       len(items),
		Items:       items,
	}
	if err := saveItemDB(db); err != nil {
		jsonError(w, http.StatusInternalServerError, "保存物品库失败: "+err.Error())
		return
	}
	sv.events.Add(inst.Name, "items", "从游戏刷新物品库：%d 项（中文名 %d 项，%s）", len(items), named, dump.Path)
	jsonOK(w, map[string]any{
		"ok": true, "message": msg, "count": len(items), "named": named,
		"path": dump.Path, "game_build_id": build,
	})
}

// handleModPlayers GET /api/instances/{name}/players
// 走 mod 的 whojson 命令，返回结构化在线玩家（给物品对话框的下拉框用）
func (sv *Server) handleModPlayers(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	if !hasGiveMod(inst) {
		jsonError(w, http.StatusBadRequest, "该实例未安装 gspanel 扩展命令 mod")
		return
	}
	ok, msg, err := sv.runModVerb(inst, "whojson", nil, 10*time.Second)
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	if !ok {
		jsonError(w, http.StatusConflict, msg)
		return
	}
	var players []ModPlayer
	if err := json.Unmarshal([]byte(msg), &players); err != nil {
		jsonError(w, http.StatusBadGateway, "mod 返回格式不正确（可能是旧版 mod，请重新安装扩展命令）: "+err.Error())
		return
	}
	if players == nil {
		players = []ModPlayer{}
	}
	jsonOK(w, map[string]any{"players": players})
}

// ModPlayer 在线玩家（mod 索引结果）
type ModPlayer struct {
	Name  string `json:"name"`
	UID   string `json:"uid"`
	Steam string `json:"steam"`
}
