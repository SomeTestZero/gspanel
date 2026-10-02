package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------- 文件夹式 Mod 管理（七日杀等：Mods/<目录>/ModInfo.xml）----------
//
// 启用 = Mods/<名>；禁用 = 移到 Mods.disabled/<名>（游戏只扫 Mods/ 一级子目录）。
// 安装来源：zip/tar.gz 上传、URL 直链下载、NexusMods（见 nexus.go）。

// ModEntry 面板展示用的 mod 条目
type ModEntry struct {
	Name          string `json:"name"`
	NameZh        string `json:"name_zh,omitempty"`
	Dir           string `json:"dir"`
	Version       string `json:"version"`
	Author        string `json:"author"`
	Description   string `json:"description"`
	DescriptionZh string `json:"description_zh,omitempty"`
	Website       string `json:"website"`
	Enabled       bool   `json:"enabled"`
	Size          int64  `json:"size"`
}

// enrichModZh 填充名称/描述的中文翻译（缓存 + 内置词典，没有则留空）
func enrichModZh(mods []ModEntry) {
	for i := range mods {
		mods[i].NameZh = modLangLookup(mods[i].Name)
		mods[i].DescriptionZh = modLangLookup(mods[i].Description)
	}
}

// modInfoXML 解析 ModInfo.xml：<Name value="x"/> 或旧式 <Name>x</Name>
type modInfoXML struct {
	Items []struct {
		XMLName xml.Name
		Value   string `xml:"value,attr"`
		Text    string `xml:",chardata"`
	} `xml:",any"`
}

// parseModInfoFile 读取单个 ModInfo.xml
func parseModInfoFile(path string) ModEntry {
	out := ModEntry{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var doc modInfoXML
	if err := xml.Unmarshal(data, &doc); err != nil {
		return out
	}
	get := func(key string) string {
		for _, it := range doc.Items {
			if strings.EqualFold(it.XMLName.Local, key) {
				if v := strings.TrimSpace(it.Value); v != "" {
					return v
				}
				return strings.TrimSpace(it.Text)
			}
		}
		return ""
	}
	out.Name = get("DisplayName")
	if out.Name == "" {
		out.Name = get("Name") // 旧式 mod 没有 DisplayName
	}
	out.Version = get("Version")
	out.Author = get("Author")
	out.Description = strings.TrimSpace(get("Description"))
	out.Website = get("Website")
	return out
}

// modDirs 启用/禁用目录（禁用目录与启用目录同级，<dir>.disabled）
func modDirs(inst *Instance, tmpl *GameTemplate) (active, disabled string) {
	base := inst.Dir + "/" + tmpl.ModManager.Dir
	return base, base + ".disabled"
}

// validModName mod 目录名安全校验（防路径穿越）
func validModName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, "/\\\x00") && !strings.HasPrefix(name, ".")
}

// listModsAt 列出启用/禁用目录下的 mod（测试用纯路径版本）
func listModsAt(active, disabled string) []ModEntry {
	out := []ModEntry{}
	scan := func(dir string, enabled bool) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() || !validModName(e.Name()) {
				continue
			}
			modDir := dir + "/" + e.Name()
			info := parseModInfoFile(modDir + "/ModInfo.xml")
			if info.Name == "" {
				info.Name = e.Name()
			}
			info.Dir = e.Name()
			info.Enabled = enabled
			info.Size = dirSize(modDir)
			out = append(out, info)
		}
	}
	scan(active, true)
	scan(disabled, false)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Enabled != out[j].Enabled {
			return out[i].Enabled
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.Walk(root, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi != nil && !fi.IsDir() {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// findModRoots 在解压目录里找所有含 ModInfo.xml 的 mod 根（最外层优先，最多下探 4 层）
func findModRoots(root string) []string {
	var roots []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > 4 {
			return
		}
		if fileExistsCI(dir, "ModInfo.xml") {
			roots = append(roots, dir)
			return // 已是 mod 根，不再向下
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				walk(dir+"/"+e.Name(), depth+1)
			}
		}
	}
	walk(root, 0)
	return roots
}

func fileExistsCI(dir, name string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(e.Name(), name) {
			return true
		}
	}
	return false
}

// extractArchive 解压 zip / tar.gz 到 dest（防 zip-slip，限制总量防解压炸弹）
func extractArchive(archivePath, dest string) error {
	const maxTotal = 8 << 30 // 解压总量上限 8GB
	const maxFiles = 200000
	var total int64
	count := 0
	safeJoin := func(name string) (string, error) {
		name = strings.ReplaceAll(name, "\\", "/")
		if name == "" || strings.HasPrefix(name, "/") {
			return "", fmt.Errorf("压缩包内路径非法: %s", name)
		}
		clean := filepath.Clean(name)
		if clean == "." {
			return dest, nil // 根目录条目（"./"）
		}
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return "", fmt.Errorf("压缩包内路径非法: %s", name)
		}
		full := filepath.Join(dest, clean)
		if !strings.HasPrefix(full, filepath.Clean(dest)+string(os.PathSeparator)) {
			return "", fmt.Errorf("压缩包内路径非法: %s", name)
		}
		return full, nil
	}
	tally := func(n int64) error {
		total += n
		count++
		if total > maxTotal || count > maxFiles {
			return fmt.Errorf("压缩包过大（解压上限 %d 文件 / %d 字节）", maxFiles, maxTotal)
		}
		return nil
	}

	switch {
	case strings.HasSuffix(strings.ToLower(archivePath), ".zip"):
		zr, err := zip.OpenReader(archivePath)
		if err != nil {
			return fmt.Errorf("不是有效的 zip 文件（rar/7z 请先在本地转成 zip）: %w", err)
		}
		defer zr.Close()
		for _, f := range zr.File {
			dst, err := safeJoin(f.Name)
			if err != nil {
				return err
			}
			if f.FileInfo().IsDir() {
				if err := os.MkdirAll(dst, 0755); err != nil {
					return err
				}
				continue
			}
			if err := tally(int64(f.UncompressedSize64)); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				return err
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			w, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
			if err != nil {
				rc.Close()
				return err
			}
			_, err = io.Copy(w, rc)
			rc.Close()
			w.Close()
			if err != nil {
				return err
			}
		}
		return nil
	case strings.HasSuffix(strings.ToLower(archivePath), ".tar.gz"), strings.HasSuffix(strings.ToLower(archivePath), ".tgz"):
		f, err := os.Open(archivePath)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("不是有效的 tar.gz 文件: %w", err)
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			dst, err := safeJoin(hdr.Name)
			if err != nil {
				return err
			}
			switch hdr.Typeflag {
			case tar.TypeDir:
				if err := os.MkdirAll(dst, 0755); err != nil {
					return err
				}
			case tar.TypeReg:
				if err := tally(hdr.Size); err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
					return err
				}
				w, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)|0644)
				if err != nil {
					return err
				}
				_, err = io.Copy(w, tr)
				w.Close()
				if err != nil {
					return err
				}
			}
		}
	default:
		lower := strings.ToLower(archivePath)
		if strings.HasSuffix(lower, ".7z") || strings.HasSuffix(lower, ".rar") {
			return extractWith7z(archivePath, dest)
		}
		return fmt.Errorf("仅支持 .zip / .tar.gz / .tgz / .7z / .rar 压缩包")
	}
}

// extractWith7z 用外部 7z/7za 解 7z/rar 等格式（需服务器装 p7zip-full）。
// 解包前用 `7z l -slt` 校验成员：拒绝绝对路径、.. 逃逸、符号链接/加密条目，防路径穿越。
func extractWith7z(archivePath, dest string) error {
	bin := ""
	for _, c := range []string{"7z", "7za"} {
		if p, err := exec.LookPath(c); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		return fmt.Errorf("服务器未安装 7z（sudo apt install p7zip-full），7z/rar 请先在本地转成 zip 再上传")
	}
	listing, err := exec.Command(bin, "l", "-slt", "--", archivePath).Output()
	if err != nil {
		return fmt.Errorf("读取压缩包目录失败: %w", err)
	}
	if err := check7zEntries(string(listing)); err != nil {
		return err
	}
	cmd := exec.Command(bin, "x", "-y", "-o"+dest, "--", archivePath)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("7z 解压失败: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	// 双保险：解出来的符号链接直接拒绝（正常 mod 不会用）
	var bad string
	_ = filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			bad = p
			return fs.SkipAll
		}
		return nil
	})
	if bad != "" {
		return fmt.Errorf("压缩包含符号链接，已拒绝: %s", bad)
	}
	return nil
}

// check7zEntries 校验 `7z l -slt` 输出的成员条目（只看分隔线 ---------- 之后的条目块）
func check7zEntries(listing string) error {
	parts := strings.SplitN(listing, "\n----------\n", 2)
	if len(parts) != 2 {
		return fmt.Errorf("无法解析压缩包目录（7z l -slt 输出异常）")
	}
	for _, block := range strings.Split(parts[1], "\n\n") {
		fields := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			if k, v, ok := strings.Cut(line, " = "); ok {
				fields[k] = v
			}
		}
		path := fields["Path"]
		if path == "" {
			continue
		}
		if strings.HasPrefix(path, "/") {
			return fmt.Errorf("压缩包内路径非法: %s", path)
		}
		for _, seg := range strings.Split(strings.ReplaceAll(path, "\\", "/"), "/") {
			if seg == ".." {
				return fmt.Errorf("压缩包内路径非法: %s", path)
			}
		}
		if fields["Symbolic Link"] == "+" || strings.Contains(fields["Attributes"], "L") {
			return fmt.Errorf("压缩包含符号链接条目，已拒绝: %s", path)
		}
		if fields["Encrypted"] == "+" {
			return fmt.Errorf("压缩包含加密条目，无法处理: %s", path)
		}
	}
	return nil
}

// isNestedArchive 整合包里再套一层的压缩包（面板会再解一层找 mod 根）
func isNestedArchive(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".zip") || strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz")
}

// stripArchiveExt 去掉压缩包后缀作目标目录名
func stripArchiveExt(name string) string {
	lower := strings.ToLower(name)
	for _, ext := range []string{".tar.gz", ".tgz", ".zip"} {
		if strings.HasSuffix(lower, ext) {
			return name[:len(name)-len(ext)]
		}
	}
	return name
}

// expandNestedArchives 解开目录里的嵌套压缩包（解到同级目录，只解一层；
// 解压失败的保留原文件，由调用方在「未识别内容」里报告，不再静默丢弃）。
// 返回成功展开的压缩包相对名。
func expandNestedArchives(root, dir string, log io.Writer) []string {
	var archives []string
	var walk func(d string, depth int)
	walk = func(d string, depth int) {
		entries, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range entries {
			p := d + "/" + e.Name()
			if e.IsDir() {
				if depth < 3 {
					walk(p, depth+1)
				}
				continue
			}
			if isNestedArchive(e.Name()) {
				archives = append(archives, p)
			}
		}
	}
	walk(dir, 0)

	var expanded []string
	for _, a := range archives {
		target := filepath.Dir(a) + "/" + stripArchiveExt(filepath.Base(a))
		for i := 2; ; i++ {
			if _, err := os.Stat(target); err != nil {
				break
			}
			target = filepath.Dir(a) + "/" + stripArchiveExt(filepath.Base(a)) + fmt.Sprintf("-%d", i)
		}
		if err := os.MkdirAll(target, 0755); err != nil {
			continue
		}
		rel, _ := filepath.Rel(root, a)
		if err := extractArchive(a, target); err != nil {
			_ = os.RemoveAll(target) // 留下原压缩包进「未识别内容」报告
			continue
		}
		_ = os.Remove(a)
		expanded = append(expanded, rel)
		fmt.Fprintf(log, "展开嵌套压缩包：%s\n", rel)
	}
	return expanded
}

// unhandledContents 不在任何 mod 根下的文件（含没展开成功的嵌套压缩包），最多报 30 项
func unhandledContents(root string, roots []string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		for _, r := range roots {
			rp, _ := filepath.Rel(root, r)
			rp = filepath.ToSlash(rp)
			if rel == rp || strings.HasPrefix(rel, rp+"/") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() {
			return nil
		}
		if len(out) < 30 {
			out = append(out, rel)
		} else {
			out = append(out, "…")
			return fs.SkipAll
		}
		return nil
	})
	return out
}

// installArchiveToMods 解压压缩包并把其中的 mod 装入 activeDir。
// 返回：安装的 mod 目录名、包里未识别（未安装）的内容；整合包里的嵌套压缩包会再解一层。
func installArchiveToMods(archivePath, activeDir string, log io.Writer) (installed, skipped []string, err error) {
	tmp, err := os.MkdirTemp("", "gsp-mod-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmp)
	if err := extractArchive(archivePath, tmp); err != nil {
		return nil, nil, err
	}
	expandNestedArchives(tmp, tmp, log)
	roots := findModRoots(tmp)
	if len(roots) == 0 {
		skipped = unhandledContents(tmp, nil)
		extra := ""
		if len(skipped) > 0 {
			extra = fmt.Sprintf("；包内未识别内容：%s", strings.Join(skipped, "、"))
		}
		return nil, skipped, fmt.Errorf("压缩包里没找到 ModInfo.xml，不是有效的七日杀 Mod（整包需是 mod 目录或其上层）%s", extra)
	}
	if err := os.MkdirAll(activeDir, 0755); err != nil {
		return nil, nil, err
	}
	installed = []string{}
	for _, root := range roots {
		base := filepath.Base(root)
		if root == tmp { // 整包就是 mod 根：用 ModInfo 里的名字
			if info := parseModInfoFile(root + "/ModInfo.xml"); info.Name != "" {
				base = sanitizeDirName(info.Name)
			} else {
				base = "mod-" + time.Now().Format("060102-150405")
			}
		}
		if !validModName(base) {
			base = sanitizeDirName(base)
		}
		dst := activeDir + "/" + base
		if _, err := os.Stat(dst); err == nil {
			fmt.Fprintf(log, "覆盖已有同名 mod：%s\n", base)
			if err := os.RemoveAll(dst); err != nil {
				return installed, nil, fmt.Errorf("清理旧 mod %s 失败: %w", base, err)
			}
		}
		if err := copyTree(root, dst); err != nil {
			return installed, nil, fmt.Errorf("安装 mod %s 失败: %w", base, err)
		}
		info := parseModInfoFile(dst + "/ModInfo.xml")
		fmt.Fprintf(log, "已安装：%s%s\n", base, versionSuffix(info.Version))
		installed = append(installed, base)
	}
	skipped = unhandledContents(tmp, roots)
	chownRecursive(activeDir)
	return installed, skipped, nil
}

func versionSuffix(v string) string {
	if v == "" {
		return ""
	}
	return "（版本 " + v + "）"
}

// sanitizeDirName 把任意名字变成安全目录名
func sanitizeDirName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == ' ':
			return '_'
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r >= 0x80:
			return r
		}
		return -1
	}, s)
	s = strings.Trim(s, ".")
	if s == "" {
		return "mod"
	}
	return s
}

// copyTree 递归复制目录（跨文件系统安全，os.Rename 不可用时的通用方案）
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s 不是目录", src)
	}
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := src + "/" + e.Name()
		d := dst + "/" + e.Name()
		if e.IsDir() {
			if err := copyTree(s, d); err != nil {
				return err
			}
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			continue // 不复制符号链接，防指向包外
		}
		data, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, data, 0644); err != nil {
			return err
		}
	}
	return nil
}

// downloadToFile 下载 URL 到临时文件，返回文件路径（调用方负责清理）
func downloadToFile(ctx context.Context, rawURL string, log io.Writer) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("仅支持 http/https 直链")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "GSPanel/1.0")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	suffix := ".zip"
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "gzip") {
		suffix = ".tar.gz"
	}
	f, err := os.CreateTemp("", "gsp-dl-*"+suffix)
	if err != nil {
		return "", err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(resp.Body, 4<<30))
	if err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("下载中断: %w", err)
	}
	fmt.Fprintf(log, "下载完成：%s（%s）\n", u.Host, humanBytes(uint64(n)))
	return f.Name(), nil
}

// ---------- HTTP handlers ----------

func (sv *Server) modMgrOf(w http.ResponseWriter, r *http.Request) (*Instance, *GameTemplate, bool) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return nil, nil, false
	}
	tmpl := sv.templateOf(inst)
	if tmpl == nil || tmpl.ModManager == nil {
		jsonError(w, http.StatusBadRequest, "该模板不支持 Mod 管理")
		return nil, nil, false
	}
	return inst, tmpl, true
}

func (sv *Server) handleModList(w http.ResponseWriter, r *http.Request) {
	inst, tmpl, ok := sv.modMgrOf(w, r)
	if !ok {
		return
	}
	active, disabled := modDirs(inst, tmpl)
	mods := listModsAt(active, disabled)
	enrichModZh(mods)
	jsonOK(w, map[string]any{
		"mods":          mods,
		"dir":           tmpl.ModManager.Dir,
		"disabled_dir":  tmpl.ModManager.Dir + ".disabled",
		"needs_restart": true,
	})
}

// handleModUpload 上传 zip/tar.gz 安装（multipart 字段 file）
func (sv *Server) handleModUpload(w http.ResponseWriter, r *http.Request) {
	inst, tmpl, ok := sv.modMgrOf(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<30)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		jsonError(w, http.StatusBadRequest, "上传失败: "+err.Error())
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		jsonError(w, http.StatusBadRequest, "缺少 file 字段")
		return
	}
	defer file.Close()
	tmp, err := os.CreateTemp("", "gsp-up-*"+filepath.Ext(hdr.Filename))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		jsonError(w, http.StatusInternalServerError, "保存上传文件失败: "+err.Error())
		return
	}
	tmp.Close()
	active, disabled := modDirs(inst, tmpl)
	installed, skipped, err := installArchiveToMods(tmp.Name(), active, io.Discard)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(skipped) > 0 {
		sv.events.Add(inst.Name, "mod", "上传安装 Mod：%s（另有 %d 项未识别未安装）", strings.Join(installed, "、"), len(skipped))
	} else {
		sv.events.Add(inst.Name, "mod", "上传安装 Mod：%s", strings.Join(installed, "、"))
	}
	jsonOK(w, map[string]any{"ok": true, "installed": installed, "skipped": skipped, "mods": listModsAt(active, disabled)})
}

// handleModInstallURL 从直链 zip 在线安装（GitHub Release 等），走任务队列
func (sv *Server) handleModInstallURL(w http.ResponseWriter, r *http.Request) {
	inst, tmpl, ok := sv.modMgrOf(w, r)
	if !ok {
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if !decodeJSON(w, r, &req) || strings.TrimSpace(req.URL) == "" {
		jsonError(w, http.StatusBadRequest, "缺少 url")
		return
	}
	t := sv.tasks.Run("mod-install", inst.Name, "从 URL 安装 Mod", func(ctx context.Context, log io.Writer, task *Task) error {
		return sv.modInstallFromURL(ctx, log, inst, tmpl, strings.TrimSpace(req.URL))
	})
	jsonOK(w, t)
}

func (sv *Server) modInstallFromURL(ctx context.Context, log io.Writer, inst *Instance, tmpl *GameTemplate, rawURL string) error {
	fmt.Fprintf(log, "下载 %s ...\n", rawURL)
	dl, err := downloadToFile(ctx, rawURL, log)
	if err != nil {
		return err
	}
	defer os.Remove(dl)
	active, _ := modDirs(inst, tmpl)
	installed, skipped, err := installArchiveToMods(dl, active, log)
	if err != nil {
		return err
	}
	if len(skipped) > 0 {
		fmt.Fprintf(log, "未识别未安装 %d 项：%s\n", len(skipped), strings.Join(skipped, "、"))
	}
	sv.events.Add(inst.Name, "mod", "在线安装 Mod：%s", strings.Join(installed, "、"))
	fmt.Fprintln(log, "安装完成，重启服务器后生效（客户端需装同版本 mod）")
	return nil
}

// handleModToggle 启用/禁用（Mods <-> Mods.disabled 移动）
func (sv *Server) handleModToggle(w http.ResponseWriter, r *http.Request) {
	inst, tmpl, ok := sv.modMgrOf(w, r)
	if !ok {
		return
	}
	var req struct {
		Name   string `json:"name"`
		Enable bool   `json:"enable"`
	}
	if !decodeJSON(w, r, &req) || !validModName(req.Name) {
		jsonError(w, http.StatusBadRequest, "mod 名非法")
		return
	}
	active, disabled := modDirs(inst, tmpl)
	src, dst := active+"/"+req.Name, disabled+"/"+req.Name
	if !req.Enable {
		src, dst = dst, src
	}
	if _, err := os.Stat(src); err != nil {
		jsonError(w, http.StatusNotFound, "mod 不存在")
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := os.Stat(dst); err == nil {
		jsonError(w, http.StatusConflict, "目标位置已有同名 mod")
		return
	}
	if err := os.Rename(src, dst); err != nil {
		jsonError(w, http.StatusInternalServerError, "移动失败: "+err.Error())
		return
	}
	chownRecursive(filepath.Dir(dst))
	sv.events.Add(inst.Name, "mod", "%s Mod：%s", map[bool]string{true: "启用", false: "禁用"}[req.Enable], req.Name)
	jsonOK(w, map[string]any{"ok": true})
}

// handleModDelete 删除 mod（两个目录都找）
func (sv *Server) handleModDelete(w http.ResponseWriter, r *http.Request) {
	inst, tmpl, ok := sv.modMgrOf(w, r)
	if !ok {
		return
	}
	name := r.URL.Query().Get("name")
	if !validModName(name) {
		jsonError(w, http.StatusBadRequest, "mod 名非法")
		return
	}
	active, disabled := modDirs(inst, tmpl)
	removed := false
	for _, dir := range []string{active, disabled} {
		p := dir + "/" + name
		if _, err := os.Stat(p); err == nil {
			if err := os.RemoveAll(p); err != nil {
				jsonError(w, http.StatusInternalServerError, "删除失败: "+err.Error())
				return
			}
			removed = true
		}
	}
	if !removed {
		jsonError(w, http.StatusNotFound, "mod 不存在")
		return
	}
	sv.events.Add(inst.Name, "mod", "删除 Mod：%s", name)
	jsonOK(w, map[string]any{"ok": true})
}
