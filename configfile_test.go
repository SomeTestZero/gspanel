package main

import (
	"strings"
	"testing"
)

const xmlSample = `<?xml version="1.0"?>
<ServerSettings>
	<!-- GENERAL -->
	<property name="ServerName"						value="My Game Host"/>		<!-- name shown in browser -->
	<property name="TelnetEnabled"					value="false"/>
	<property name="MaxSpawnedZombies"				value="64" />
</ServerSettings>
`

func TestParseXMLKV(t *testing.T) {
	vals := parseXMLKV(xmlSample)
	if vals["ServerName"] != "My Game Host" {
		t.Fatalf("ServerName = %q", vals["ServerName"])
	}
	if vals["TelnetEnabled"] != "false" {
		t.Fatalf("TelnetEnabled = %q", vals["TelnetEnabled"])
	}
	if vals["MaxSpawnedZombies"] != "64" {
		t.Fatalf("MaxSpawnedZombies = %q", vals["MaxSpawnedZombies"])
	}
}

func TestSubstituteXMLValues(t *testing.T) {
	out := substituteXMLValues(xmlSample, map[string]string{
		"ServerName":    `新"服" & <玩家>`,
		"TelnetEnabled": "true",
		"NewKey":        "v1",
	})
	if !strings.Contains(out, `<!-- name shown in browser -->`) {
		t.Fatal("注释未保留")
	}
	if !strings.Contains(out, `value="新&quot;服&quot; &amp; &lt;玩家&gt;"`) {
		t.Fatalf("实体转义/就地替换失败:\n%s", out)
	}
	if !strings.Contains(out, `name="TelnetEnabled"					value="true"`) {
		t.Fatalf("布尔替换失败:\n%s", out)
	}
	if !strings.Contains(out, `<property name="NewKey"`) || strings.Index(out, "NewKey") > strings.Index(out, "</ServerSettings>") {
		t.Fatalf("缺失键未插到闭合标签前:\n%s", out)
	}
	// 未提交的键不受影响
	if !strings.Contains(out, `name="MaxSpawnedZombies"				value="64"`) {
		t.Fatalf("无关键被改动:\n%s", out)
	}
	// 往返一致
	vals := parseXMLKV(out)
	if vals["NewKey"] != "v1" || vals["ServerName"] != `新"服" & <玩家>` {
		t.Fatalf("往返解析不一致: %+v", vals)
	}
}
