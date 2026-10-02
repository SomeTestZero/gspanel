package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// ---------- NexusMods 本地 Mod 目录缓存（data/nexus-mods.json）----------
//
// 思路：全量拉一次（9000+ 条，80/批，后台任务）缓存到本地，浏览/搜索/筛选/排序/翻页
// 全在本地做（瞬时、无翻页地狱）；要新数据时点「同步」再全量刷新（译文缓存可复用）。

const nexusBatchSize = 80 // GraphQL 单次上限（实测）

type nexusCatalogFile struct {
	SyncedAt time.Time        `json:"synced_at"`
	Total    int              `json:"total"` // 同步时 Nexus 的总数
	Items    []map[string]any `json:"items"`
}

func nexusCatalogPath() string { return DataDir + "/nexus-mods.json" }

func loadNexusCatalog() nexusCatalogFile {
	var f nexusCatalogFile
	data, err := os.ReadFile(nexusCatalogPath())
	if err != nil {
		return f
	}
	_ = json.Unmarshal(data, &f)
	return f
}

func saveNexusCatalog(f nexusCatalogFile) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp := nexusCatalogPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, nexusCatalogPath())
}

// nexusGameFilter 所有七日杀 mod 的基础过滤
func nexusGameFilter() map[string]any {
	return map[string]any{"gameDomainName": []map[string]any{{"value": nexusGameDomain}}}
}

// syncNexusCatalog 全量同步：createdAt 正/反两遍翻页补齐（分页并列可能漏），按 mod_id 去重
func (sv *Server) syncNexusCatalog(ctx context.Context, log io.Writer) (int, error) {
	sv.state.mu.RLock()
	apiKey := sv.state.NexusAPIKey
	sv.state.mu.RUnlock()
	if apiKey == "" {
		return 0, fmt.Errorf("未配置 NexusMods API key（设置页填写）")
	}

	catalog := loadNexusCatalog()
	byID := map[int64]map[string]any{}
	for _, it := range catalog.Items {
		if id, ok := it["mod_id"].(float64); ok {
			byID[int64(id)] = it
		}
	}

	total := 0
	newCount := 0
	for _, dir := range []string{"ASC", "DESC"} {
		offset := 0
		for {
			if err := ctx.Err(); err != nil {
				return 0, fmt.Errorf("同步被取消（已完成 %d 条）", len(byID))
			}
			items, t, err := fetchModNodes(apiKey, nexusGameFilter(), "createdAt", dir, offset, nexusBatchSize)
			if err != nil {
				return len(byID), fmt.Errorf("拉取第 %d 条起失败: %w", offset, err)
			}
			total = t
			before := len(byID)
			for _, it := range items {
				id, _ := it["mod_id"].(int64)
				if id != 0 {
					byID[id] = it
				}
			}
			newCount += len(byID) - before
			offset += nexusBatchSize
			fmt.Fprintf(log, "已拉取 %d / %d 条（本批新增 %d）\n", len(byID), total, len(byID)-before)
			if len(items) < nexusBatchSize || offset >= total {
				break
			}
			select {
			case <-ctx.Done():
				return len(byID), fmt.Errorf("同步被取消（已完成 %d 条）", len(byID))
			case <-time.After(120 * time.Millisecond): // 温和限速
			}
		}
		if len(byID) >= total {
			break
		}
	}

	out := make([]map[string]any, 0, len(byID))
	for _, it := range byID {
		out = append(out, it)
	}
	f := nexusCatalogFile{SyncedAt: time.Now(), Total: total, Items: out}
	if err := saveNexusCatalog(f); err != nil {
		return len(out), fmt.Errorf("保存本地目录失败: %w", err)
	}
	fmt.Fprintf(log, "同步完成：%d 个 mod 已缓存到本地（Nexus 总数 %d）\n", len(out), total)
	if total > len(out) {
		fmt.Fprintf(log, "提示：仍有 %d 个未入库（多为排序并列导致的漏页），可再点一次「同步」补齐\n", total-len(out))
	}
	return len(out), nil
}

// enrichCatalog 读取时补齐译文与版本提示（分类/名称走翻译缓存+内置词典）
func enrichCatalog(items []map[string]any, gameVersion string) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		cp := map[string]any{}
		for k, v := range it {
			cp[k] = v
		}
		if zh := modLangLookup(fmt.Sprint(cp["category"])); zh != "" {
			cp["category_zh"] = zh
		}
		if zh := modLangLookup(fmt.Sprint(cp["name"])); zh != "" {
			cp["name_zh"] = zh
		}
		cp["version_hint"] = gameVersionHint(
			fmt.Sprint(cp["name"], " ", cp["summary"], " ", cp["version"]), gameVersion)
		out = append(out, cp)
	}
	return out
}

// ---------- HTTP handlers ----------

// handleNexusCatalog 读取本地目录（附译文/版本提示）
func (sv *Server) handleNexusCatalog(w http.ResponseWriter, r *http.Request) {
	f := loadNexusCatalog()
	jsonOK(w, map[string]any{
		"items":     enrichCatalog(f.Items, r.URL.Query().Get("game_version")),
		"synced_at": f.SyncedAt,
		"total":     f.Total,
	})
}

// handleNexusCatalogSync 全量同步（后台任务）
func (sv *Server) handleNexusCatalogSync(w http.ResponseWriter, r *http.Request) {
	t := sv.tasks.Run("mod-sync", "-", "同步 NexusMods Mod 目录", func(ctx context.Context, log io.Writer, task *Task) error {
		_, err := sv.syncNexusCatalog(ctx, log)
		return err
	})
	jsonOK(w, t)
}
