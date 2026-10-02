package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pathSpecFixture 造一个带 kv 配置文件的临时实例，测试路径解析
func pathSpecFixture(t *testing.T, cfg string) (*Instance, *GameTemplate, string) {
	t.Helper()
	dir := t.TempDir()
	if cfg != "" {
		if err := os.WriteFile(filepath.Join(dir, "serverconfig.txt"), []byte(cfg), 0644); err != nil {
			t.Fatal(err)
		}
	}
	inst := &Instance{Name: "t1", Dir: dir}
	tmpl := &GameTemplate{Configs: []ConfigSpec{{Path: "serverconfig.txt", Format: "kv"}}}
	home := filepath.Join(dir, "home") // 测试注入的假 games 家目录
	return inst, tmpl, home
}

func TestResolvePathSpec(t *testing.T) {
	inst, tmpl, home := pathSpecFixture(t, "GameWorld=Navezgane\nGameName=MyGame\nWorldGenSeed=MyGame\n")
	cases := []struct {
		spec string
		want string // 相对 home/instDir 的期望后缀；err 非空则期望报错
		err  string
	}{
		{"serverconfig.txt", inst.Dir + "/serverconfig.txt", ""},
		{"Saves/x", inst.Dir + "/Saves/x", ""},
		{"~/.local/share/7DaysToDie/Saves", home + "/.local/share/7DaysToDie/Saves", ""},
		{"~/.local/share/7DaysToDie/Saves/{config:GameWorld}/{config:GameName}",
			home + "/.local/share/7DaysToDie/Saves/Navezgane/MyGame", ""},
		{"~/.local/share/7DaysToDie/Saves/{config:WorldGenSeed}/{config:GameName}",
			home + "/.local/share/7DaysToDie/Saves/MyGame/MyGame", ""},
		{"", "", "路径不能为空"},
		{"../etc", "", "不允许包含 .."},
		{"a/../b", "", "不允许包含 .."},
		{"/etc/passwd", "", "须为相对实例目录或以 ~/ 开头"},
		{"~/.local/{config:NoSuchKey}", "", "配置键 NoSuchKey 无值"},
		{"~/.local/{config:GameName}x{bad}", "", "占位符语法非法"},
	}
	for _, c := range cases {
		got, err := resolvePathSpec(home, inst, tmpl, c.spec)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("resolvePathSpec(%q) 错误 = %v，期望包含 %q", c.spec, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolvePathSpec(%q) 报错: %v", c.spec, err)
			continue
		}
		if got != c.want {
			t.Errorf("resolvePathSpec(%q) = %q，期望 %q", c.spec, got, c.want)
		}
	}
}

// 配置值把路径带飞（含 ..）必须报错；普通怪值也只能落在 home 之下
func TestResolvePathSpecRejectsHostileConfigValue(t *testing.T) {
	inst, tmpl, home := pathSpecFixture(t, "GameWorld=../../etc\n")
	if _, err := resolvePathSpec(home, inst, tmpl, "~/.local/{config:GameWorld}"); err == nil {
		t.Error("配置值含 .. 时应报错")
	}
	inst2, tmpl2, home2 := pathSpecFixture(t, "GameWorld=/etc/passwd\n")
	got, err := resolvePathSpec(home2, inst2, tmpl2, "~/.local/{config:GameWorld}")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got != filepath.Join(home2, ".local/etc/passwd") {
		t.Errorf("展开结果 = %q，应被收在 home 之下", got)
	}
}

// 配置文件缺失时占位符应报错而不是静默展开
func TestResolvePathSpecMissingConfigFile(t *testing.T) {
	inst, tmpl, home := pathSpecFixture(t, "")
	if _, err := resolvePathSpec(home, inst, tmpl, "Saves/{config:GameName}"); err == nil {
		t.Error("配置文件缺失时应报错")
	}
}

func TestCheckBackupMembers(t *testing.T) {
	ok := [][]string{
		{"Saves/", "Saves/Navezgane/", "Saves/Navezgane/MyGame/main.ttp", "serverconfig.xml"},
		{"/home/games/.local/share/7DaysToDie/Saves/Navezgane/MyGame/region/r.0.0.7rg"},
		{"/home/games"},
		{"./Saves/x"},
	}
	for _, members := range ok {
		if err := checkBackupMembers(members); err != nil {
			t.Errorf("checkBackupMembers(%v) = %v，期望通过", members, err)
		}
	}
	bad := [][]string{
		{"../etc/passwd"},
		{"Saves/../../etc/passwd"},
		{"/etc/passwd"},
		{"/home/gamesx/evil"},
		{"/home/games/../root/.ssh/authorized_keys"},
	}
	for _, members := range bad {
		if err := checkBackupMembers(members); err == nil {
			t.Errorf("checkBackupMembers(%v) 应报错", members)
		}
	}
}

// backupEntries：实例内成员用相对名（旧备份兼容），实例外成员用绝对名
func TestBackupEntriesMemberNames(t *testing.T) {
	inst, tmpl, _ := pathSpecFixture(t, "GameName=x\n")
	tmpl.BackupPaths = []string{"serverconfig.txt", "Saves", "~/gspanel-test-nonexistent-xyz", "!Mods/*/Resources"}
	if err := os.MkdirAll(inst.Dir+"/Saves", 0755); err != nil {
		t.Fatal(err)
	}
	entries, err := backupEntries(inst, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"serverconfig.txt", "Saves"} // ~/... 在真家目录下不存在，跳过
	if len(entries) != len(want) {
		t.Fatalf("backupEntries = %+v，期望 %d 项", entries, len(want))
	}
	for i, e := range entries {
		if e.Member != want[i] {
			t.Errorf("member[%d] = %q，期望 %q", i, e.Member, want[i])
		}
	}
}
