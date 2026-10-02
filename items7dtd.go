package main

import (
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 七日杀物品库 / 在线玩家 / 给物品（telnet 通道） ----------
//
// 与帕鲁（palworlditems.go，物品库靠游戏 mod 导出）不同，七日杀的物品直接从
// 游戏文件解析，随时是最新：
//   - Data/Config/items.xml            物品 ID + Group/Stacknumber
//   - Data/Config/Localization.csv     物品中文名（schinese 列，键=物品 ID）
//   - Mods/*/Config/items.xml          mod 新增物品（同目录 Localization.csv 补译名）
// 玩家列表/give 走 telnet 控制台（telnet.go）：`lp` 列玩家，`give <玩家/entityid> <物品> <数量> [品质]`。

const (
	dtdItemsRelPath = "Data/Config/items.xml"
	dtdLocRelPath   = "Data/Config/Localization.csv"
	dtdItemCacheTTL = time.Minute // 双保险：签名失效 + TTL（mod 目录增删才需要重扫）
)

// dtdCatRule 前缀/后缀 → 分类（key 稳定即可，label 给前端显示）
type dtdCatRule struct {
	match  string
	prefix bool
	key    string
	label  string
}

var dtdCatRules = []dtdCatRule{
	{"Parts", false, "parts", "零件"},
	{"SkillMagazine", false, "books", "书籍/杂志"},
	{"meleeWpn", true, "melee", "近战武器"},
	{"meleeTool", true, "tools", "工具"},
	{"gun", true, "guns", "枪械"},
	{"flamethrower", true, "guns", "枪械"},
	{"ammoBundle", true, "ammo", "弹药"},
	{"ammo", true, "ammo", "弹药"},
	{"thrown", true, "thrown", "投掷/爆炸物"},
	{"explosive", true, "thrown", "投掷/爆炸物"},
	{"mod", true, "mods", "模组/配件"},
	{"food", true, "food", "食物"},
	{"drink", true, "drink", "饮料"},
	{"medical", true, "meds", "药品"},
	{"drug", true, "meds", "药品"},
	{"firstAid", true, "meds", "药品"},
	{"painkillers", true, "meds", "药品"},
	{"vitamins", true, "meds", "药品"},
	{"herbal", true, "meds", "药品"},
	{"grandpas", true, "meds", "药品"},
	{"clothing", true, "clothing", "服饰"},
	{"armor", true, "armor", "护甲"},
	{"vehicle", true, "vehicles", "载具"},
	{"book", true, "books", "书籍/杂志"},
	{"resource", true, "materials", "材料"},
	{"electric", true, "electric", "电路/陷阱"},
	{"trap", true, "electric", "电路/陷阱"},
	{"cnt", true, "stations", "工作台/容器"},
	{"forge", true, "stations", "工作台/容器"},
	{"questItem", true, "quest", "任务物品"},
	{"super", true, "special", "特殊道具"},
	{"casino", true, "special", "特殊道具"},
	{"dice", true, "special", "特殊道具"},
}

// dtdInternalPrefixes 非玩家物品（僵尸拳头、任务奖励占位、测试物品等），不进「给物品」列表
var dtdInternalPrefixes = []string{
	"TEST_ITEM", "meleeHand", "ammoProjectile", "questReward", "tier",
	"unit", "qt", "giveXP", "biome", "adminT", "adminGiveBuff",
}

func dtdItemCategory(id, group string) string {
	for _, r := range dtdCatRules {
		if (r.prefix && strings.HasPrefix(id, r.match)) || (!r.prefix && strings.HasSuffix(id, r.match)) {
			return r.key
		}
	}
	// 名字没有可识别前缀时按 items.xml 的 Group 属性兜底
	switch {
	case strings.Contains(group, "Weapon"), strings.Contains(group, "Ammo"):
		return "guns"
	case strings.Contains(group, "Tool"):
		return "tools"
	case strings.Contains(group, "Food"), strings.Contains(group, "Cooking"):
		return "food"
	case strings.Contains(group, "Book"):
		return "books"
	case strings.Contains(group, "Resource"):
		return "materials"
	case strings.Contains(group, "Clothing"), strings.Contains(group, "Apparel"):
		return "clothing"
	}
	return "misc"
}

func dtdIsInternalItem(id string) bool {
	for _, p := range dtdInternalPrefixes {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	return false
}

// ---------- items.xml / Localization.csv 解析 ----------

type dtdRawItem struct {
	id       string
	group    string
	stackMax int
}

// parseDtdItemsFile 流式解析 items.xml 的顶层 <item>（name + 直接子属性 Group/Stacknumber）
func parseDtdItemsFile(path string) ([]dtdRawItem, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := xml.NewDecoder(f)
	var out []dtdRawItem
	var cur *dtdRawItem
	depth, itemDepth := 0, -1
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch t.Name.Local {
			case "item":
				if cur != nil {
					break // 嵌套 <item>（effect_group 里的引用）不当新物品
				}
				name := ""
				for _, a := range t.Attr {
					if a.Name.Local == "name" {
						name = strings.TrimSpace(a.Value)
					}
				}
				if name != "" {
					cur = &dtdRawItem{id: name}
					itemDepth = depth
				} else {
					cur = nil
				}
			case "property":
				if cur == nil || depth != itemDepth+1 {
					break
				}
				name, value := "", ""
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "name":
						name = a.Value
					case "value":
						value = a.Value
					}
				}
				switch name {
				case "Group":
					cur.group = value
				case "Stacknumber":
					if n, e := strconv.Atoi(strings.TrimSpace(value)); e == nil {
						cur.stackMax = n
					}
				}
			}
		case xml.EndElement:
			if t.Name.Local == "item" && cur != nil && depth == itemDepth {
				out = append(out, *cur)
				cur = nil
				itemDepth = -1
			}
			depth--
		}
	}
	return out, nil
}

// parseDtdLocNames 从 Localization.csv 取 wanted 集合里物品的中文名（schinese 优先，english 兜底）
func parseDtdLocNames(path string, wanted map[string]bool) map[string][2]string {
	out := map[string][2]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	rd := csv.NewReader(f)
	rd.FieldsPerRecord = -1
	rd.LazyQuotes = true
	header, err := rd.Read()
	if err != nil {
		return out
	}
	colKey, colZh, colEn := -1, -1, -1
	for i, h := range header {
		switch strings.ToLower(strings.TrimSpace(h)) {
		case "key":
			colKey = i
		case "schinese":
			colZh = i
		case "english":
			colEn = i
		}
	}
	if colKey < 0 {
		return out
	}
	get := func(rec []string, i int) string {
		if i < 0 || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}
	for {
		rec, err := rd.Read()
		if err != nil {
			break
		}
		key := get(rec, colKey)
		if key == "" || !wanted[key] {
			continue
		}
		out[key] = [2]string{get(rec, colZh), get(rec, colEn)}
	}
	return out
}

// ---------- 物品库（带缓存） ----------

type dtdCacheEntry struct {
	sig      string
	loadedAt time.Time
	db       *ItemDB
}

var dtdItemCache sync.Map // inst.Dir -> *dtdCacheEntry

// dtdItemsSignature 相关文件的 mtime/size 指纹（mod 增删、游戏更新都会失效）
func dtdItemsSignature(dir string) string {
	var paths []string
	add := func(p string) { paths = append(paths, p) }
	add(filepath.Join(dir, dtdItemsRelPath))
	add(filepath.Join(dir, dtdLocRelPath))
	if mods, _ := filepath.Glob(filepath.Join(dir, "Mods", "*", "Config", "items.xml")); len(mods) > 0 {
		paths = append(paths, mods...)
	}
	if locs, _ := filepath.Glob(filepath.Join(dir, "Mods", "*", "Config", "Localization.csv")); len(locs) > 0 {
		paths = append(paths, locs...)
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "%s|%d|%d;", p, fi.ModTime().UnixNano(), fi.Size())
		}
	}
	return b.String()
}

// dtdItemDB 取实例物品库：签名一致走缓存，变化则重扫游戏文件
func dtdItemDB(inst *Instance) (*ItemDB, error) {
	sig := dtdItemsSignature(inst.Dir)
	if v, ok := dtdItemCache.Load(inst.Dir); ok {
		e := v.(*dtdCacheEntry)
		if e.sig == sig && time.Since(e.loadedAt) < dtdItemCacheTTL {
			return e.db, nil
		}
	}
	db, err := dtdBuildItemDB(inst.Dir)
	if err != nil {
		return nil, err
	}
	dtdItemCache.Store(inst.Dir, &dtdCacheEntry{sig: sig, loadedAt: time.Now(), db: db})
	return db, nil
}

func dtdBuildItemDB(dir string) (*ItemDB, error) {
	basePath := filepath.Join(dir, dtdItemsRelPath)
	raw, err := parseDtdItemsFile(basePath)
	if err != nil {
		return nil, fmt.Errorf("读取物品表失败（实例是否已安装游戏？）: %w", err)
	}
	seen := map[string]bool{}
	type entry struct {
		dtdRawItem
		fromMod bool
	}
	all := make([]entry, 0, len(raw))
	for _, it := range raw {
		if seen[it.id] {
			continue
		}
		seen[it.id] = true
		all = append(all, entry{it, false})
	}
	mods, _ := filepath.Glob(filepath.Join(dir, "Mods", "*", "Config", "items.xml"))
	sort.Strings(mods)
	for _, mp := range mods {
		modItems, err := parseDtdItemsFile(mp)
		if err != nil {
			continue // 单个 mod 的物品表坏了不影响其余
		}
		for _, it := range modItems {
			if seen[it.id] {
				continue
			}
			seen[it.id] = true
			all = append(all, entry{it, true})
		}
	}

	// 译名：本体 Localization.csv 打底，mod 的覆盖
	wanted := map[string]bool{}
	for _, it := range all {
		wanted[it.id] = true
	}
	names := parseDtdLocNames(filepath.Join(dir, dtdLocRelPath), wanted)
	for _, mp := range mods {
		locPath := filepath.Join(filepath.Dir(mp), "Localization.csv")
		for k, v := range parseDtdLocNames(locPath, wanted) {
			names[k] = v
		}
	}

	items := make([]ItemEntry, 0, len(all))
	for _, it := range all {
		if dtdIsInternalItem(it.id) {
			continue
		}
		zh, en := "", ""
		if n, ok := names[it.id]; ok {
			zh, en = n[0], n[1]
		}
		name := zh
		if name == "" {
			name = en
		}
		if name == "" {
			name = it.id
		}
		items = append(items, ItemEntry{
			ID:       it.id,
			Name:     name,
			Type:     dtdItemCategory(it.id, it.group),
			Type2:    en,
			MaxStack: it.stackMax,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })

	// 分类中文名（前端 chips 显示用）
	catLabels := map[string]string{"misc": "其它"}
	for _, r := range dtdCatRules {
		catLabels[r.key] = r.label
	}
	return &ItemDB{
		Source:      "游戏文件 items.xml + Localization.csv",
		Culture:     "zh-Hans",
		GeneratedAt: time.Now().Format(time.RFC3339),
		Count:       len(items),
		Items:       items,
		CatLabels:   catLabels,
	}, nil
}

// dtdCleanResp 去掉 telnet 回显的执行日志行（`INF Executing command ...`）
func dtdCleanResp(resp string) string {
	var lines []string
	for _, l := range strings.Split(resp, "\n") {
		if strings.Contains(l, "Executing command") {
			continue
		}
		lines = append(lines, l)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// ---------- 在线玩家（telnet `lp`） ----------

// lp 输出示例（V3.2）：
//
//  0. id=171, 玩家名, pos=(x, y, z), rot=(...), remote=True, health=100, deaths=0,
//     zombies=0, players=0, score=0, level=1, steamid=7656119..., ip=1.2.3.4, ping=30
//     Total of 1 in the game
//
// 玩家名可能含空格/逗号，所以用「下一个 key=」做名字右边界
var (
	dtdPlayerRe = regexp.MustCompile(`id=(\d+),\s*(.*?),\s*(?:pos|remote|health|deaths|zombies|players|score|level|steamid|ip|ping)=`)
	dtdSteamRe  = regexp.MustCompile(`steamid=(\d+)`)
)

func parseDtdPlayers(resp string) []ModPlayer {
	var out []ModPlayer
	for _, line := range strings.Split(resp, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "Executing command") ||
			strings.HasPrefix(line, "Total of") || strings.HasPrefix(line, "***") {
			continue
		}
		m := dtdPlayerRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		p := ModPlayer{UID: m[1], Name: strings.TrimSpace(m[2])}
		if sm := dtdSteamRe.FindStringSubmatch(line); sm != nil {
			p.Steam = sm[1]
		}
		out = append(out, p)
	}
	return out
}

// dtdListPlayers 读在线玩家（给物品对话框下拉框用）
func dtdListPlayers(inst *Instance, tmpl *GameTemplate) ([]ModPlayer, error) {
	resp, err := telnetExecConfig(inst, tmpl, "lp")
	if err != nil {
		return nil, err
	}
	players := parseDtdPlayers(resp)
	if players == nil {
		players = []ModPlayer{}
	}
	return players, nil
}

// ---------- give ----------

var dtdGiveErrRe = regexp.MustCompile(`(?i)not found|invalid|unknown|error|unable|cannot|no such|failed`)

// giveKindOf 实例支持的「给物品」通道：mod=帕鲁 UE4SS mod 文件队列，telnet=七日杀控制台，""=不支持
func giveKindOf(inst *Instance, tmpl *GameTemplate) string {
	if tmpl == nil {
		return ""
	}
	if tmpl.ID == "palworld" && hasGiveMod(inst) {
		return "mod"
	}
	if tmpl.ID == "7dtd" && tmpl.RCON != nil && tmpl.RCON.Type == "telnet" {
		return "telnet"
	}
	return ""
}

// dtdGiveOK give 响应是否算成功：telnet 没有稳定的成功标记，
// 只能按已知错误措辞排除（失败如 "Playername or entity id not found."）
func dtdGiveOK(resp string) bool { return !dtdGiveErrRe.MatchString(resp) }

// dtdGive 给指定玩家物品：`give <玩家名/entityid> <物品ID> <数量> [品质]`（物品掉在玩家面前）
func dtdGive(inst *Instance, tmpl *GameTemplate, player, item string, count, quality int) (string, error) {
	player = strings.TrimSpace(player)
	if player == "" {
		return "", fmt.Errorf("玩家名不能为空")
	}
	if len(player) > 64 || strings.ContainsAny(player, "\r\n\x00") {
		return "", fmt.Errorf("玩家名不合法")
	}
	if count < 1 || count > 10000 {
		return "", fmt.Errorf("数量必须是 1～10000 的整数")
	}
	if quality < 0 || quality > 6 {
		return "", fmt.Errorf("品质必须是 1～6（或留空）")
	}
	if strings.ContainsAny(item, " \r\n\x00") {
		return "", fmt.Errorf("物品 ID 不合法")
	}
	db, err := dtdItemDB(inst)
	if err != nil {
		return "", err
	}
	found := false
	for _, it := range db.Items {
		if it.ID == item {
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("物品 ID 不在物品库中，请从列表选择")
	}
	cmd := fmt.Sprintf("give %s %s %d", player, item, count)
	if quality > 0 {
		cmd += " " + strconv.Itoa(quality)
	}
	resp, err := telnetExecConfig(inst, tmpl, cmd)
	if err != nil {
		return "", err
	}
	return dtdCleanResp(resp), nil
}
