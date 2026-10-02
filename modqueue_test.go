package main

import (
	"strings"
	"testing"
)

func TestValidateModCommand(t *testing.T) {
	cases := []struct {
		verb  string
		args  []string
		valid bool
	}{
		{"whojson", nil, true}, {"hello", nil, true}, {"who", []string{"extra"}, false},
		{"give", []string{"玩家 名", "Wood", "1"}, true},
		{"give", []string{"UID", "Wood", "10000"}, true},
		{"give", []string{"UID", "Wood", "10001"}, false},
		{"give", []string{"UID", "Wood", "0"}, false},
		{"give", []string{"UID", "Wood", "-1"}, false},
		{"give", []string{"UID", "Wood", "1.5"}, false},
		{"give", []string{"UID", "Wood", "1e3"}, false},
		{"give", []string{"UID", "Wood", ""}, false},
		{"give", []string{"UID", "Wood"}, false},
		{"give", []string{"", "Wood", "1"}, false},
		{"give", []string{"x\ny", "Wood", "1"}, false},
		{"give", []string{"x\x00y", "Wood", "1"}, false},
		{"give", []string{"UID", "../Wood", "1"}, false},
		{"give", []string{"UID", "Wood", "9999999999999999999999"}, false},
		{"giveexp", []string{"UID", "10000000"}, true},
		{"giveexp", []string{"UID", "10000001"}, false},
		{"probe", []string{"throw"}, false},
	}
	for _, tt := range cases {
		t.Run(tt.verb+strings.Join(tt.args, "_"), func(t *testing.T) {
			if err := validateModCommand(tt.verb, tt.args); (err == nil) != tt.valid {
				t.Fatalf("valid=%t err=%v", tt.valid, err)
			}
		})
	}
}

func TestParseModResult(t *testing.T) {
	cases := []struct {
		data        string
		matched, ok bool
		message     string
	}{
		{"gsp2:new\nOK\n已到账\n", true, true, "已到账"},
		{"gsp2:new\nFAIL\n背包已满\n", true, false, "背包已满"},
		{"gsp2:old\nOK\n迟到结果\n", false, false, ""},
		{"OK\n旧版响应\n", false, false, ""},
		{"gsp2:new\nOK", false, false, ""},
		{"gsp2:new\n", false, false, ""},
		{"gsp2:new\nUNKNOWN\nfoo", false, false, ""},
	}
	for _, tt := range cases {
		m, ok, msg := parseModResult([]byte(tt.data), "new")
		if m != tt.matched || ok != tt.ok || msg != tt.message {
			t.Fatalf("%q: got %v %v %q", tt.data, m, ok, msg)
		}
	}
}

func TestModVersionMatchesAsset(t *testing.T) {
	data, err := palworldAsset("mod/scripts/main.lua")
	if err != nil {
		t.Fatal(err)
	}
	match := modVersionRe.FindSubmatch(data)
	if len(match) != 2 || string(match[1]) != modVersion {
		t.Fatal("modVersion differs from embedded Lua")
	}
}
