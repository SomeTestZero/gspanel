package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGameAssetReplaceDoesNotTruncateHardlink(t *testing.T) {
	// 无需 root：测试时 chown 回当前身份，不依赖宿主机 games 用户。
	oldUID, oldGID := gamesUID, gamesGID
	gamesUID, gamesGID = uint32(os.Getuid()), uint32(os.Getgid())
	t.Cleanup(func() { gamesUID, gamesGID = oldUID, oldGID })
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "live.so"), filepath.Join(dir, "install.so")
	if err := os.WriteFile(src, []byte("old mapped contents"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(src, dst); err != nil {
		t.Fatal(err)
	}
	if err := writeGameAsset(dst, strings.NewReader("new asset"), 0755); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(src)
	newData, _ := os.ReadFile(dst)
	if string(old) != "old mapped contents" || string(newData) != "new asset" {
		t.Fatalf("overwrote source inode: src=%q dst=%q", old, newData)
	}
	a, _ := os.Stat(src)
	b, _ := os.Stat(dst)
	if os.SameFile(a, b) {
		t.Fatal("asset still aliases live inode")
	}
	if b.Mode().Perm() != 0755 {
		t.Fatal("lost executable permission")
	}
}

func TestLinuxReflectionLayoutAssets(t *testing.T) {
	data, err := palworldAsset("layouts/MemberVariableLayout.ini")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"ArrayDim = 0x34", "ElementSize = 0x38", "Names = 0x48"} {
		if !strings.Contains(string(data), expected) {
			t.Errorf("missing %s", expected)
		}
	}
	data, err = palworldAsset("layouts/VTableLayout.ini")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "__vecDelDtor\n__linuxDeletingDtor\n") {
		t.Error("Linux double destructor slot missing")
	}
}
