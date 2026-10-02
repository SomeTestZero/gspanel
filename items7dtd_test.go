package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 写一个最小七日杀实例目录（items.xml + Localization.csv + 可选 mod）
func writeDtdFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	must := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(dir, "Data/Config/items.xml"), `<?xml version="1.0" encoding="UTF-8"?>
<item_info>
<item name="gunHandgunT1Pistol">
	<property name="Stacknumber" value="1"/>
	<property name="Group" value="Ammo/Weapons,Ammo,Ranged Weapons"/>
	<effect_group name="gunHandgunT1Pistol">
		<passive_effect name="EntityDamage" operation="base_set" value="32"/>
	</effect_group>
</item>
<item name="resourceWood">
	<property name="Stacknumber" value="6000"/>
	<property name="Group" value="Resources"/>
</item>
<item name="ammo9mmBulletBall">
	<property name="Stacknumber" value="300"/>
	<property name="Group" value="Ammo/Weapons,Ammo,Ranged Weapons"/>
</item>
<item name="bladesSkillMagazine">
	<property name="Stacknumber" value="25"/>
</item>
<item name="meleeHandZombie01">
	<property name="Stacknumber" value="1"/>
</item>
<item name="TEST_ITEM_00"/>
<item name="tier01AgilityWeapons"/>
<item name="gunHandgunT1PistolParts">
	<property name="Stacknumber" value="500"/>
</item>
</item_info>`)
	must(filepath.Join(dir, "Data/Config/Localization.csv"),
		"Key,File,Type,UsedInMainMenu,NoTranslate,KeepLoaded,english,Context / Alternate Text,schinese\n"+
			"gunHandgunT1Pistol,items,Item,,,x,Pistol,,手枪\n"+
			"resourceWood,items,Item,,,x,Wood,,木头\n"+
			"ammo9mmBulletBall,items,Item,,,x,9mm Ammo,,9毫米弹药\n"+
			"bladesSkillMagazine,items,Item,,,x,Knife Guy,,刀剑客\n")
	// mod：新增物品 + 自带译名（列名与本体表不同）
	must(filepath.Join(dir, "Mods/weapons/Config/items.xml"), `<?xml version="1.0" encoding="UTF-8"?>
<item_info>
<item name="gunModernRifle">
	<property name="Stacknumber" value="1"/>
</item>
<item name="resourceWood"/>
</item_info>`)
	must(filepath.Join(dir, "Mods/weapons/Config/Localization.csv"),
		"Key,File,Type,UsedInMainMenu,NoTranslate,Schinese\n"+
			"gunModernRifle,items,Item,,,现代步枪\n")
	return dir
}

func TestDtdParseItemsFile(t *testing.T) {
	dir := writeDtdFixture(t)
	items, err := parseDtdItemsFile(filepath.Join(dir, "Data/Config/items.xml"))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]dtdRawItem{}
	for _, it := range items {
		byID[it.id] = it
	}
	if len(items) != 8 {
		t.Fatalf("解析出 %d 个物品，应为 8：%+v", len(items), items)
	}
	p := byID["gunHandgunT1Pistol"]
	if p.stackMax != 1 || p.group != "Ammo/Weapons,Ammo,Ranged Weapons" {
		t.Errorf("手枪属性解析错误: %+v", p)
	}
	if byID["resourceWood"].stackMax != 6000 {
		t.Errorf("木头 Stacknumber=%d，应为 6000", byID["resourceWood"].stackMax)
	}
}

func TestDtdBuildItemDB(t *testing.T) {
	dir := writeDtdFixture(t)
	db, err := dtdBuildItemDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ItemEntry{}
	for _, it := range db.Items {
		byID[it.ID] = it
	}
	// 内部/测试物品过滤
	for _, id := range []string{"meleeHandZombie01", "TEST_ITEM_00", "tier01AgilityWeapons"} {
		if _, ok := byID[id]; ok {
			t.Errorf("%s 是内部物品，不应出现在物品库", id)
		}
	}
	// 中文名：本体 Localization.csv
	if got := byID["gunHandgunT1Pistol"]; got.Name != "手枪" || got.MaxStack != 1 || got.Type != "guns" {
		t.Errorf("手枪条目错误: %+v", got)
	}
	if got := byID["resourceWood"]; got.Name != "木头" || got.Type != "materials" {
		t.Errorf("木头条目错误: %+v", got)
	}
	// 无中文名 → 兜底英文名（本表未收录）→ 再兜底 ID
	if got := byID["ammo9mmBulletBall"]; got.Name != "9毫米弹药" {
		t.Errorf("弹药名错误: %+v", got)
	}
	if got := byID["gunHandgunT1PistolParts"]; got.Name != "gunHandgunT1PistolParts" || got.Type != "parts" {
		t.Errorf("零件条目错误（应按 ID 兜底 + Parts 后缀分类）: %+v", got)
	}
	// 杂志分类
	if got := byID["bladesSkillMagazine"]; got.Type != "books" {
		t.Errorf("杂志分类错误: %+v", got)
	}
	// mod 物品 + mod 自带译名
	if got := byID["gunModernRifle"]; got.Name != "现代步枪" {
		t.Errorf("mod 物品名错误: %+v", got)
	}
	// mod 里重复的本体物品不重复出现
	n := 0
	for _, it := range db.Items {
		if it.ID == "resourceWood" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("resourceWood 出现 %d 次，应去重", n)
	}
	if db.Count != len(db.Items) || db.Count != 6 {
		t.Errorf("Count=%d len=%d，应为 6", db.Count, len(db.Items))
	}
}

func TestDtdItemCategory(t *testing.T) {
	cases := map[string]string{
		"gunShotgunT2PumpShotgun":     "guns",
		"meleeWpnBladeT3Machete":      "melee",
		"meleeToolPickT2SteelPickaxe": "tools",
		"ammo762mmBulletBall":         "ammo",
		"ammoBundle762mm":             "ammo",
		"thrownAmmoPipeBomb":          "thrown",
		"foodBoiledMeat":              "food",
		"drinkJarGoldenRodTea":        "drink",
		"firstAidKit":                 "meds",
		"armorAssassinBoots":          "armor",
		"vehicleMinibikePlaceable":    "vehicles",
		"resourceForgedSteel":         "materials",
		"modGunScope02":               "mods",
		"bookArtOfMiningVol1":         "books",
		"cntCabinetOld":               "stations",
		"superCorn":                   "special",
		"weirdCustomThing":            "misc",
	}
	for id, want := range cases {
		if got := dtdItemCategory(id, ""); got != want {
			t.Errorf("dtdItemCategory(%q)=%q，应为 %q", id, got, want)
		}
	}
	// 名字不可识别时按 Group 属性兜底
	if got := dtdItemCategory("weirdCustomThing", "Tools/Traps"); got != "tools" {
		t.Errorf("Group 兜底失败: %q", got)
	}
}

// lp 输出解析：玩家名含空格、忽略日志/统计行、提取 steamid
func TestParseDtdPlayers(t *testing.T) {
	resp := `2026-10-01T22:24:36 69078.663 INF Executing command 'lp' by Telnet from 127.0.0.1:50802
   0. id=171, Player One, pos=(1066.9, 37.8, -2340.8), rot=(0.0, -117.7, 0.0), remote=True, health=73, deaths=12, zombies=47, players=0, score=776, level=31, steamid=76561198012345678, ip=1.2.3.4, ping=25
   1. id=203, Bob the Great, pos=(1.0, 2.0, 3.0), rot=(0.0, 0.0, 0.0), remote=True, health=100, deaths=0, zombies=0, players=0, score=0, level=1, steamid=76561198087654321, ip=5.6.7.8, ping=40
Total of 2 in the game`
	players := parseDtdPlayers(resp)
	if len(players) != 2 {
		t.Fatalf("解析出 %d 个玩家，应为 2：%+v", len(players), players)
	}
	if players[0].UID != "171" || players[0].Name != "Player One" || players[0].Steam != "76561198012345678" {
		t.Errorf("玩家1解析错误: %+v", players[0])
	}
	if players[1].Name != "Bob the Great" {
		t.Errorf("含空格玩家名解析错误: %+v", players[1])
	}
	if got := parseDtdPlayers("Total of 0 in the game"); len(got) != 0 {
		t.Errorf("空服应返回空列表: %+v", got)
	}
}

func TestDtdGiveOK(t *testing.T) {
	for resp, want := range map[string]bool{
		"Playername or entity id not found.":                     false,
		"Invalid item name 'xxx'":                                false,
		"Giving item 'gunHandgunT1Pistol' to player Player One.": true,
		"1 item given to Player One":                             true,
	} {
		if got := dtdGiveOK(resp); got != want {
			t.Errorf("dtdGiveOK(%q)=%t，应为 %t", resp, got, want)
		}
	}
}

// 真机物品表冒烟（有 7days 实例才跑）：解析数量级 + 关键物品中文名
func TestDtdRealItemsSmoke(t *testing.T) {
	const dir = "/home/games/instances/7days"
	if _, err := os.Stat(filepath.Join(dir, dtdItemsRelPath)); err != nil {
		t.Skip("本机没有 7days 实例，跳过")
	}
	db, err := dtdBuildItemDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(db.Items) < 700 {
		t.Fatalf("真机物品库只有 %d 项，疑似解析不全", len(db.Items))
	}
	byID := map[string]ItemEntry{}
	for _, it := range db.Items {
		byID[it.ID] = it
	}
	for id, wantName := range map[string]string{
		"gunHandgunT1Pistol": "手枪",
		"resourceWood":       "木头",
	} {
		if got := byID[id]; got.Name != wantName {
			t.Errorf("%s 名称=%q，应为 %q", id, got.Name, wantName)
		}
	}
	if _, ok := byID["meleeHandZombie01"]; ok {
		t.Error("僵尸拳头不应出现在物品库")
	}
}
