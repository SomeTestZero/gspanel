package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- NexusMods 在线浏览/安装（七日杀 mod 主要发布渠道）----------
//
// 数据面分工：
//   - 浏览/搜索/分类：GraphQL v2（api.nexusmods.com/v2/graphql，支持分页/排序/分类过滤）
//   - 文件列表/下载直链/账号校验：v1 REST（download_link 需 Premium；免费账号用 nxm 的 key/expires）
// 响应字段名以官方 SDK（node-nexus-api）与实测为准，解析保持宽容以兼容字段增减。

const (
	nexusAPIBase    = "https://api.nexusmods.com/v1"
	nexusGQL        = "https://api.nexusmods.com/v2/graphql"
	nexusGameDomain = "7daystodie" // Vortex 扩展里的 Nexus 域名
)

// NxmLink nxm://7daystodie/mods/<mod>/files/<file>?key=..&expires=..
type NxmLink struct {
	ModID   int64
	FileID  int64
	Key     string
	Expires int64
}

// parseNxmLink 解析 nxm:// 链接（免费账号从 Nexus 下载页「Mod Manager 下载」按钮复制）
func parseNxmLink(s string) (NxmLink, error) {
	var out NxmLink
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(strings.ToLower(s), "nxm://") {
		return out, fmt.Errorf("不是 nxm:// 链接")
	}
	u, err := url.Parse("nxm://" + s[len("nxm://"):])
	if err != nil {
		return out, fmt.Errorf("nxm 链接解析失败: %w", err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	// mods/<modID>/files/<fileID>
	if len(parts) != 4 || parts[0] != "mods" || parts[2] != "files" {
		return out, fmt.Errorf("nxm 链接格式不对，应为 nxm://7daystodie/mods/<id>/files/<id>")
	}
	if out.ModID, err = strconv.ParseInt(parts[1], 10, 64); err != nil {
		return out, fmt.Errorf("nxm 链接 mod id 非法")
	}
	if out.FileID, err = strconv.ParseInt(parts[3], 10, 64); err != nil {
		return out, fmt.Errorf("nxm 链接 file id 非法")
	}
	q := u.Query()
	out.Key = q.Get("key")
	if v := q.Get("expires"); v != "" {
		out.Expires, _ = strconv.ParseInt(v, 10, 64)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// nexusGet 调 Nexus v1 API 并解码 JSON 到 out
func nexusGet(apiKey, path string, query url.Values, out any) error {
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("未配置 NexusMods API key（设置页填写，nexusmods.com → 我的账号 → API 访问）")
	}
	u := nexusAPIBase + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("apikey", strings.TrimSpace(apiKey))
	req.Header.Set("Application-Name", "GSPanel")
	req.Header.Set("Application-Version", "1.0")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("请求 NexusMods 失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("NexusMods API key 无效，请在设置页核对")
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("NexusMods 拒绝访问（免费账号下载直链需从下载页复制 nxm 链接后粘贴安装）")
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("NexusMods 资源不存在")
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("NexusMods 返回 HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("NexusMods 响应解析失败: %w", err)
	}
	return nil
}

// nexusGraphQL 调 GraphQL v2（查询大部分免鉴权，带 key 更稳）
func nexusGraphQL(apiKey, query string, vars map[string]any, out any) error {
	payload, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, err := http.NewRequest(http.MethodPost, nexusGQL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("apikey", strings.TrimSpace(apiKey))
	}
	req.Header.Set("Application-Name", "GSPanel")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("请求 NexusMods GraphQL 失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("NexusMods GraphQL 响应解析失败: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("NexusMods GraphQL: %s", truncate(envelope.Errors[0].Message, 200))
	}
	return json.Unmarshal(envelope.Data, out)
}

// ---------- 浏览 / 搜索 / 筛选 ----------

// nexusSortMap 前端排序键 -> GraphQL 排序字段
var nexusSortMap = map[string]string{
	"downloads": "downloads",
	"created":   "createdAt",
	"updated":   "updatedAt",
	"endorse":   "endorsements",
	"relevance": "relevance",
	"name":      "name",
}

// nexusModsBrowse 浏览/搜索 mod（分页 + 排序 + 分类过滤）
// 返回归一化条目、总数；gameVersion 非空时附加版本提示
// fetchModNodes 按指定排序拉取一页 mod（归一化条目 + 总数）。GraphQL 单次上限 80 条（实测）
func fetchModNodes(apiKey string, filter map[string]any, sortField, sortDir string, offset, count int) ([]map[string]any, int, error) {
	if count <= 0 || count > 80 {
		count = 20
	}
	if offset < 0 {
		offset = 0
	}
	if sortDir != "ASC" {
		sortDir = "DESC"
	}
	const gql = `query($filter: ModsFilter, $sort: [ModsSort!], $offset: Int, $count: Int) {
		mods(filter: $filter, sort: $sort, offset: $offset, count: $count) {
			totalCount
			nodes { modId name summary author category downloads endorsements version pictureUrl thumbnailUrl createdAt updatedAt tags { name } }
		}
	}`
	vars := map[string]any{
		"filter": filter,
		"sort":   []map[string]any{{sortField: map[string]any{"direction": sortDir}}},
		"offset": offset,
		"count":  count,
	}
	var raw struct {
		Mods struct {
			TotalCount int `json:"totalCount"`
			Nodes      []struct {
				ModID        int64  `json:"modId"`
				Name         string `json:"name"`
				Summary      string `json:"summary"`
				Author       string `json:"author"`
				Category     string `json:"category"`
				Downloads    int64  `json:"downloads"`
				Endorsements int64  `json:"endorsements"`
				Version      string `json:"version"`
				PictureURL   string `json:"pictureUrl"`
				ThumbURL     string `json:"thumbnailUrl"`
				CreatedAt    string `json:"createdAt"`
				UpdatedAt    string `json:"updatedAt"`
				Tags         []struct {
					Name string `json:"name"`
				} `json:"tags"`
			} `json:"nodes"`
		} `json:"mods"`
	}
	if err := nexusGraphQL(apiKey, gql, vars, &raw); err != nil {
		return nil, 0, err
	}
	items := make([]map[string]any, 0, len(raw.Mods.Nodes))
	for _, n := range raw.Mods.Nodes {
		tags := make([]string, 0, len(n.Tags))
		for _, t := range n.Tags {
			tags = append(tags, t.Name)
		}
		img := n.ThumbURL
		if img == "" {
			img = n.PictureURL
		}
		items = append(items, map[string]any{
			"mod_id":       n.ModID,
			"name":         n.Name,
			"summary":      n.Summary,
			"author":       n.Author,
			"category":     n.Category,
			"downloads":    n.Downloads,
			"endorsements": n.Endorsements,
			"version":      n.Version,
			"picture_url":  img,
			"created_at":   n.CreatedAt,
			"updated_at":   n.UpdatedAt,
			"tags":         tags,
			"side":         modSideHint(n.Name, n.Summary, tags),
		})
	}
	return items, raw.Mods.TotalCount, nil
}

// nexusModsBrowse 浏览/搜索 mod（分页 + 排序 + 分类过滤）；gameVersion 非空时附加版本提示
func nexusModsBrowse(apiKey, query, sort, category, gameVersion string, offset, count int) ([]map[string]any, int, error) {
	field := nexusSortMap[sort]
	if field == "" {
		field = "downloads"
	}
	filter := map[string]any{
		"gameDomainName": []map[string]any{{"value": nexusGameDomain}},
	}
	if q := strings.TrimSpace(query); q != "" {
		filter["nameStemmed"] = []map[string]any{{"value": q, "op": "WILDCARD"}}
	}
	if category != "" {
		filter["categoryName"] = []map[string]any{{"value": category}}
	}
	items, total, err := fetchModNodes(apiKey, filter, field, "DESC", offset, count)
	if err != nil {
		return nil, 0, err
	}
	for _, it := range items {
		it["version_hint"] = gameVersionHint(fmt.Sprint(it["name"], " ", it["summary"], " ", it["version"]), gameVersion)
	}
	return items, total, nil
}

var (
	reServerSide = regexp.MustCompile(`server[- ]?(side|only)|dedicated server|install on (the )?server|works? on (the )?server`)
	reClientSide = regexp.MustCompile(`client[- ]?(side|only)|install on (the )?client|cosmetic|visual[- ]only`)
	reBothSide   = regexp.MustCompile(`both (the )?(client|server)|server and client|client and server|install on both`)
	// 明确的游戏版本标记：V3.2 / A21（旧 Alpha）；"version 1.0"、裸数字（多为 mod 自身版本）不认
	reGameVer = regexp.MustCompile(`(?i)\bv(\d+\.\d+)\b|\ba(\d{2})\b`)
)

// modSideHint 启发式判断 mod 端侧（Nexus 无结构化字段，仅供参考）：
// server=服务端装 | client=客户端装 | both=双端 | client?=视觉类标签推测客户端 | ""=未知
func modSideHint(name, summary string, tags []string) string {
	text := strings.ToLower(name + " " + summary + " " + strings.Join(tags, " "))
	server, client := reServerSide.MatchString(text), reClientSide.MatchString(text)
	if reBothSide.MatchString(text) || (server && client) {
		return "both"
	}
	if server {
		return "server"
	}
	if client {
		return "client"
	}
	for _, t := range tags { // 软信号：纯视觉/音效/UI 标签多为客户端 mod
		switch strings.ToLower(t) {
		case "ui", "visual", "audio", "animation", "replacer":
			return "client?"
		}
	}
	return ""
}

// gameVersionHint 从文本提取游戏版本提示（只认 V3.2/A21 这类明确标记，避免把 mod 自身版本当游戏版本）：
// match:<v>=标明与服务端同版本；maybe:<v>=标明了别的版本（需人工核对）；""=无版本信息
func gameVersionHint(text, gameVersion string) string {
	gameVersion = strings.TrimSpace(gameVersion)
	if gameVersion == "" {
		return ""
	}
	majorMinor := gameVersion
	if i := strings.Index(majorMinor, "."); i > 0 {
		rest := majorMinor[i+1:]
		if j := strings.Index(rest, "."); j > 0 {
			majorMinor = majorMinor[:i+1+j]
		}
	}
	if m := reGameVer.FindStringSubmatch(text); m != nil {
		ver := m[1]
		if ver == "" {
			ver = "A" + m[2]
		}
		if strings.EqualFold(ver, majorMinor) || strings.HasPrefix(ver, majorMinor+".") {
			return "match:" + majorMinor
		}
		return "maybe:" + ver
	}
	if strings.Contains(strings.ToLower(text), strings.ToLower(majorMinor)) {
		return "match:" + majorMinor
	}
	return ""
}

// nexusModCategories 分类表（v1 游戏信息里带，结果缓存 1 小时）
var (
	nxCatMu      sync.Mutex
	nxCatCache   []map[string]any
	nxCatCacheAt time.Time
)

func nexusModCategories(apiKey string) ([]map[string]any, error) {
	nxCatMu.Lock()
	if time.Since(nxCatCacheAt) < time.Hour && len(nxCatCache) > 0 {
		defer nxCatMu.Unlock()
		return nxCatCache, nil
	}
	nxCatMu.Unlock()
	var game struct {
		Categories []struct {
			Name string `json:"name"`
			ID   int    `json:"category_id"`
		} `json:"categories"`
	}
	if err := nexusGet(apiKey, "/games/"+nexusGameDomain+".json", nil, &game); err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, c := range game.Categories {
		out = append(out, map[string]any{"name": c.Name, "id": c.ID})
	}
	nxCatMu.Lock()
	nxCatCache, nxCatCacheAt = out, time.Now()
	nxCatMu.Unlock()
	return out, nil
}

// nexusValidate 校验 key 并返回用户信息（含 is_premium）
func nexusValidate(apiKey string) (map[string]any, error) {
	var out map[string]any
	if err := nexusGet(apiKey, "/users/validate.json", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// nexusFiles 列出某 mod 的文件（v1）
func nexusFiles(apiKey string, modID int64) ([]any, error) {
	var out struct {
		Files []any `json:"files"`
	}
	path := fmt.Sprintf("/games/%s/mods/%d/files.json", nexusGameDomain, modID)
	if err := nexusGet(apiKey, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Files, nil
}

// nexusDownloadURI 换取下载直链（Premium 直接换；免费账号带 nxm 的 key/expires）
func nexusDownloadURI(apiKey string, link NxmLink) (string, error) {
	q := url.Values{}
	if link.Key != "" {
		q.Set("key", link.Key)
	}
	if link.Expires > 0 {
		q.Set("expires", strconv.FormatInt(link.Expires, 10))
	}
	path := fmt.Sprintf("/games/%s/mods/%d/files/%d/download_link.json", nexusGameDomain, link.ModID, link.FileID)
	var out []struct {
		URI string `json:"URI"`
	}
	if err := nexusGet(apiKey, path, q, &out); err != nil {
		return "", err
	}
	if len(out) == 0 || out[0].URI == "" {
		return "", fmt.Errorf("NexusMods 未返回下载链接")
	}
	return out[0].URI, nil
}

// ---------- HTTP handlers ----------

func (sv *Server) handleSetNexusKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKey string `json:"api_key"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	sv.state.mu.Lock()
	sv.state.NexusAPIKey = strings.TrimSpace(req.APIKey)
	err := sv.state.saveLocked()
	sv.state.mu.Unlock()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true, "configured": sv.state.NexusAPIKey != ""})
}

func (sv *Server) handleNexusValidate(w http.ResponseWriter, r *http.Request) {
	info, err := nexusValidate(sv.state.NexusAPIKey)
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	jsonOK(w, map[string]any{"user": info})
}

// handleNexusBrowse Mod 商店列表：?q=&sort=&offset=&count=&category=&game_version=
func (sv *Server) handleNexusBrowse(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	count, _ := strconv.Atoi(q.Get("count"))
	items, total, err := nexusModsBrowse(sv.state.NexusAPIKey, q.Get("q"), q.Get("sort"),
		q.Get("category"), q.Get("game_version"), offset, count)
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	jsonOK(w, map[string]any{"items": items, "total": total, "offset": offset, "count": len(items)})
}

// handleNexusFilters 分类表（下拉筛选用）
func (sv *Server) handleNexusFilters(w http.ResponseWriter, r *http.Request) {
	cats, err := nexusModCategories(sv.state.NexusAPIKey)
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	jsonOK(w, map[string]any{"categories": cats})
}

func (sv *Server) handleNexusFiles(w http.ResponseWriter, r *http.Request) {
	modID, err := strconv.ParseInt(r.URL.Query().Get("mod_id"), 10, 64)
	if err != nil || modID <= 0 {
		jsonError(w, http.StatusBadRequest, "mod_id 非法")
		return
	}
	files, err := nexusFiles(sv.state.NexusAPIKey, modID)
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	// 附加版本提示（文件名/版本号里通常带游戏版本）
	gameVersion := r.URL.Query().Get("game_version")
	if gameVersion != "" {
		for _, f := range files {
			if m, ok := f.(map[string]any); ok {
				text := fmt.Sprint(m["file_name"], " ", m["name"], " ", m["version"], " ", m["mod_version"])
				m["version_hint"] = gameVersionHint(text, gameVersion)
			}
		}
	}
	jsonOK(w, map[string]any{"files": files})
}

// handleModInstallNexus 从 NexusMods 安装：body 支持 {mod_id,file_id[,key,expires]} 或 {nxm}
func (sv *Server) handleModInstallNexus(w http.ResponseWriter, r *http.Request) {
	inst, tmpl, ok := sv.modMgrOf(w, r)
	if !ok {
		return
	}
	var req struct {
		Nxm     string `json:"nxm"`
		ModID   int64  `json:"mod_id"`
		FileID  int64  `json:"file_id"`
		Key     string `json:"key"`
		Expires int64  `json:"expires"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	var link NxmLink
	if strings.TrimSpace(req.Nxm) != "" {
		var err error
		if link, err = parseNxmLink(req.Nxm); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else if req.ModID > 0 && req.FileID > 0 {
		link = NxmLink{ModID: req.ModID, FileID: req.FileID, Key: req.Key, Expires: req.Expires}
	} else {
		jsonError(w, http.StatusBadRequest, "需要 nxm 链接或 mod_id+file_id")
		return
	}
	apiKey := sv.state.NexusAPIKey
	if apiKey == "" && link.Key == "" {
		jsonError(w, http.StatusBadRequest, "未配置 NexusMods API key，且 nxm 链接里也没有临时 key")
		return
	}
	t := sv.tasks.Run("mod-install", inst.Name, fmt.Sprintf("从 NexusMods 安装 Mod #%d 文件 #%d", link.ModID, link.FileID),
		func(ctx context.Context, log io.Writer, task *Task) error {
			fmt.Fprintln(log, "换取下载直链...")
			uri, err := nexusDownloadURI(apiKey, link)
			if err != nil {
				return err
			}
			return sv.modInstallFromURL(ctx, log, inst, tmpl, uri)
		})
	jsonOK(w, t)
}
