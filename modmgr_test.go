package main

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func writeZip(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(t.TempDir(), "mod-*.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

const modInfoSample = `<xml>
  <Name value="TestMod"/>
  <DisplayName value="测试Mod"/>
  <Description value="一个测试 mod"/>
  <Author value="someone"/>
  <Version value="1.2.3"/>
  <Website value="https://example.com"/>
</xml>`

func TestParseModInfoFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ModInfo.xml")
	if err := os.WriteFile(p, []byte(modInfoSample), 0644); err != nil {
		t.Fatal(err)
	}
	info := parseModInfoFile(p)
	if info.Name != "测试Mod" || info.Version != "1.2.3" || info.Author != "someone" {
		t.Fatalf("解析错误: %+v", info)
	}
	// 旧式 <Name>文本</Name>（无 DisplayName）
	p2 := filepath.Join(dir, "old.xml")
	os.WriteFile(p2, []byte(`<xml><Name>Old Style</Name><Version>0.9</Version></xml>`), 0644)
	if got := parseModInfoFile(p2); got.Name != "Old Style" || got.Version != "0.9" {
		t.Fatalf("旧式解析错误: %+v", got)
	}
}

func TestInstallArchiveToMods(t *testing.T) {
	// 一个 zip 装两个 mod（各自一层目录 + ModInfo.xml）
	z := writeZip(t, map[string]string{
		"ModA/ModInfo.xml":  modInfoSample,
		"ModA/Config/x.xml": "<x/>",
		"ModB/ModInfo.xml":  modInfoSample,
	})
	active := filepath.Join(t.TempDir(), "Mods")
	names, skipped, err := installArchiveToMods(z, active, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("应装 2 个 mod: %v", names)
	}
	if len(skipped) != 0 {
		t.Fatalf("不应有未识别内容: %v", skipped)
	}
	for _, n := range names {
		if _, err := os.Stat(active + "/" + n + "/ModInfo.xml"); err != nil {
			t.Fatalf("mod %s 缺 ModInfo.xml: %v", n, err)
		}
	}
	// 同名覆盖安装
	z2 := writeZip(t, map[string]string{"ModA/ModInfo.xml": modInfoSample})
	if _, _, err := installArchiveToMods(z2, active, io.Discard); err != nil {
		t.Fatalf("覆盖安装失败: %v", err)
	}
	// 包里整包即 mod 根（无外层目录）：用 ModInfo 里的显示名
	z3 := writeZip(t, map[string]string{"ModInfo.xml": modInfoSample, "Config/y.xml": "<y/>"})
	names3, _, err := installArchiveToMods(z3, active, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(names3) != 1 || names3[0] != "测试Mod" {
		t.Fatalf("整包 mod 命名不对: %v", names3)
	}
	// 没有 ModInfo.xml 的包要拒绝，且报告包内未识别内容（不再静默）
	z4 := writeZip(t, map[string]string{"readme.txt": "hi"})
	if _, skipped4, err := installArchiveToMods(z4, active, io.Discard); err == nil {
		t.Fatal("缺 ModInfo.xml 应报错")
	} else if len(skipped4) != 1 || skipped4[0] != "readme.txt" {
		t.Fatalf("未识别内容报告不对: %v", skipped4)
	}
}

// 整合包里嵌套子压缩包：应再解一层装出来，其余不认识的内容进报告
func TestInstallNestedArchiveAndSkipped(t *testing.T) {
	inner := writeZip(t, map[string]string{"ModC/ModInfo.xml": modInfoSample})
	innerBytes, err := os.ReadFile(inner)
	if err != nil {
		t.Fatal(err)
	}
	z := writeZip(t, map[string]string{
		"ModA/ModInfo.xml": modInfoSample,
		"pack/ModC.zip":    string(innerBytes),
		"说明.txt":           "readme",
		"tools/辅助.exe":     "binary",
	})
	active := filepath.Join(t.TempDir(), "Mods")
	names, skipped, err := installArchiveToMods(z, active, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("应装 2 个 mod（含嵌套包里的）: %v", names)
	}
	if _, err := os.Stat(active + "/ModC/ModInfo.xml"); err != nil {
		t.Fatalf("嵌套 zip 里的 ModC 未安装: %v", err)
	}
	want := map[string]bool{"说明.txt": true, "tools/辅助.exe": true}
	if len(skipped) != len(want) {
		t.Fatalf("未识别内容 = %v，期望 %v", skipped, want)
	}
	for _, s := range skipped {
		if !want[filepath.ToSlash(s)] {
			t.Errorf("多余的未识别项: %s", s)
		}
	}
}

func TestExtractArchiveRejectsZipSlip(t *testing.T) {
	z := writeZip(t, map[string]string{"../evil.txt": "pwned"})
	if err := extractArchive(z, t.TempDir()); err == nil {
		t.Fatal("zip-slip 路径应被拒绝")
	}
}

func TestCheck7zEntries(t *testing.T) {
	good := "Path = /tmp/m.7z\nType = 7z\n\n----------\nPath = ModA\nSize = 0\nAttributes = D\nEncrypted = -\n\nPath = ModA/ModInfo.xml\nSize = 5\nAttributes = A\nEncrypted = -\n"
	if err := check7zEntries(good); err != nil {
		t.Errorf("正常目录应通过: %v", err)
	}
	bad := []string{
		"----------\nPath = ../evil\nAttributes = A\nEncrypted = -\n",
		"----------\nPath = ModA/../../evil\nAttributes = A\nEncrypted = -\n",
		"----------\nPath = /etc/passwd\nAttributes = A\nEncrypted = -\n",
		"----------\nPath = ModA/link\nAttributes = A\nSymbolic Link = +\nEncrypted = -\n",
		"----------\nPath = ModA/x\nAttributes = AL\nEncrypted = -\n",
		"----------\nPath = ModA/secret\nAttributes = A\nEncrypted = +\n",
	}
	for _, listing := range bad {
		if err := check7zEntries(listing); err == nil {
			t.Errorf("应拒绝: %q", listing)
		}
	}
	if err := check7zEntries("没有分隔线的输出"); err == nil {
		t.Error("无法解析的目录输出应报错")
	}
}

// 7z 压缩包安装：装了 p7zip 才跑；路径校验/解包链路走真 7z
func TestExtractWith7z(t *testing.T) {
	bin := ""
	for _, c := range []string{"7z", "7za"} {
		if p, err := exec.LookPath(c); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		t.Skip("未安装 7z（p7zip-full）")
	}
	src := t.TempDir()
	if err := os.WriteFile(src+"/ModInfo.xml", []byte(modInfoSample), 0644); err != nil {
		t.Fatal(err)
	}
	arch := filepath.Join(t.TempDir(), "m.7z")
	cmd := exec.Command(bin, "a", "-y", arch, "ModInfo.xml")
	cmd.Dir = src
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("创建 7z 失败: %v %s", err, out)
	}
	dest := filepath.Join(t.TempDir(), "out")
	if err := extractArchive(arch, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest + "/ModInfo.xml"); err != nil {
		t.Fatalf("7z 解包结果不对: %v", err)
	}
	// 未知格式仍要拒绝
	bad := filepath.Join(t.TempDir(), "m.rar")
	os.WriteFile(bad, []byte("not really rar"), 0644)
	if err := extractArchive(bad, t.TempDir()+"/out2"); err == nil {
		t.Error("损坏的 rar 应报错")
	}
}

func TestListModsAndToggle(t *testing.T) {
	base := t.TempDir()
	active, disabled := base+"/Mods", base+"/Mods.disabled"
	z := writeZip(t, map[string]string{"ModA/ModInfo.xml": modInfoSample})
	if _, _, err := installArchiveToMods(z, active, io.Discard); err != nil {
		t.Fatal(err)
	}
	mods := listModsAt(active, disabled)
	if len(mods) != 1 || !mods[0].Enabled || mods[0].Name != "测试Mod" || mods[0].Version != "1.2.3" {
		t.Fatalf("列表不对: %+v", mods)
	}
	// 模拟禁用/启用（与 handleModToggle 相同的移动语义）
	os.MkdirAll(disabled, 0755)
	if err := os.Rename(active+"/ModA", disabled+"/ModA"); err != nil {
		t.Fatal(err)
	}
	mods = listModsAt(active, disabled)
	if len(mods) != 1 || mods[0].Enabled {
		t.Fatalf("禁用后列表不对: %+v", mods)
	}
}

func TestValidModName(t *testing.T) {
	for _, bad := range []string{"", "..", "../x", "a/b", ".hidden", `a\b`} {
		if validModName(bad) {
			t.Fatalf("应拒绝 %q", bad)
		}
	}
	for _, good := range []string{"ModA", "测试_mod-1", "a.b"} {
		if !validModName(good) {
			t.Fatalf("应接受 %q", good)
		}
	}
}
