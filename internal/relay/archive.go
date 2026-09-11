package relay

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/ahmojo/codex-claude-transfer/internal/agent"
	"github.com/ahmojo/codex-claude-transfer/internal/bundle"
	"github.com/ahmojo/codex-claude-transfer/internal/claudehome"
	"github.com/ahmojo/codex-claude-transfer/internal/claudesessions"
	"github.com/ahmojo/codex-claude-transfer/internal/codexhome"
	"github.com/ahmojo/codex-claude-transfer/internal/safety"
	"github.com/ahmojo/codex-claude-transfer/internal/sessions"
)

type Extra struct {
	Rel    string `json:"rel"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type Archive struct {
	MinClientVersion string  `json:"min_client_version,omitempty"`
	Format           string  `json:"format"`
	Provider         string  `json:"provider"`
	Project          string  `json:"project"`
	SourcePath       string  `json:"source_path"`
	SourceOS         string  `json:"source_os"`
	NativeSHA        string  `json:"native_sha"`
	Sessions         int     `json:"sessions"`
	RawSize          int64   `json:"raw_size"`
	Extras           []Extra `json:"extras"`
}

const maxFile int64 = 2 << 30

func safeRel(rel string) error {
	if _, e := safety.CleanRelPath(rel); e != nil {
		return e
	}
	for _, seg := range strings.Split(rel, "/") {
		if strings.ContainsAny(seg, "\x00<>\"|?*") || strings.TrimRight(seg, " .") != seg {
			return fmt.Errorf("不兼容 Windows 的文件名：%s", rel)
		}
		base := strings.ToUpper(strings.SplitN(seg, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9' {
			return fmt.Errorf("Windows 保留文件名：%s", rel)
		}
	}
	return nil
}
func securePath(root, rel string) (string, error) {
	if e := safeRel(rel); e != nil {
		return "", e
	}
	cur := root
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		cur = filepath.Join(cur, p)
		st, e := os.Lstat(cur)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return "", e
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("不跟随符号链接：%s", cur)
		}
		if i < len(parts)-1 && !st.IsDir() {
			return "", fmt.Errorf("父路径不是目录：%s", cur)
		}
		if i == len(parts)-1 && !st.Mode().IsRegular() {
			return "", fmt.Errorf("目标不是普通文件：%s", cur)
		}
	}
	return cur, nil
}
func zipBytes(f *zip.File, limit int64) ([]byte, error) {
	if f.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("压缩包条目过大：%s", f.Name)
	}
	r, e := f.Open()
	if e != nil {
		return nil, e
	}
	defer r.Close()
	b, e := io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("条目超过限制")
	}
	return b, e
}
func InspectArchive(file string) (Archive, error) {
	var m Archive
	z, e := zip.OpenReader(file)
	if e != nil {
		return m, e
	}
	defer z.Close()
	if len(z.File) > 50000 {
		return m, fmt.Errorf("文件数量过多")
	}
	seen := map[string]bool{}
	entries := map[string]*zip.File{}
	var total uint64
	for _, f := range z.File {
		if e = safeRel(f.Name); e != nil {
			return m, e
		}
		key := strings.ToLower(f.Name)
		if seen[key] || !f.Mode().IsRegular() {
			return m, fmt.Errorf("重复条目或非普通文件：%s", f.Name)
		}
		seen[key] = true
		entries[f.Name] = f
		total += f.UncompressedSize64
		if total > uint64(MaxExpanded) {
			return m, fmt.Errorf("解压后超过 8 GiB")
		}
	}
	mf := entries["relay.json"]
	native := entries["sessions.codexbundle"]
	if mf == nil || native == nil {
		return m, fmt.Errorf("不是 Session Relay 压缩包")
	}
	b, e := zipBytes(mf, 2<<20)
	if e != nil {
		return m, e
	}
	if e = json.Unmarshal(b, &m); e != nil {
		return m, e
	}
	if m.Format != "session-relay-v1" || !providerOK(m.Provider) || !validID.MatchString(m.Project) || m.SourcePath == "" || m.Sessions < 1 || m.RawSize < 0 {
		return m, fmt.Errorf("快照元数据无效")
	}
	nr, e := native.Open()
	if e != nil {
		return m, e
	}
	h, n, e := hashReader(io.LimitReader(nr, MaxBundle+1))
	nr.Close()
	if e != nil || n > MaxBundle || h != m.NativeSHA {
		return m, fmt.Errorf("原生会话包校验失败")
	}
	allowed := map[string]bool{"relay.json": true, "sessions.codexbundle": true}
	for _, x := range m.Extras {
		if m.Provider != "claude" || !extraRelOK(x.Rel) || x.Size < 0 || x.Size > maxFile {
			return m, fmt.Errorf("关联文件路径或大小无效：%s", x.Rel)
		}
		name := "extras/" + x.Rel
		if allowed[name] {
			return m, fmt.Errorf("重复关联文件")
		}
		allowed[name] = true
		f := entries[name]
		if f == nil || f.UncompressedSize64 != uint64(x.Size) {
			return m, fmt.Errorf("关联文件缺失：%s", x.Rel)
		}
		r, e := f.Open()
		if e != nil {
			return m, e
		}
		h, n, e := hashReader(io.LimitReader(r, maxFile+1))
		r.Close()
		if e != nil || n != x.Size || h != x.SHA256 {
			return m, fmt.Errorf("关联文件校验失败：%s", x.Rel)
		}
	}
	if len(entries) != len(allowed) {
		return m, fmt.Errorf("存在未登记的压缩包条目")
	}
	return m, nil
}
func extraRelOK(rel string) bool {
	if safeRel(rel) != nil {
		return false
	}
	p := strings.Split(rel, "/")
	if len(p) < 3 || !validID.MatchString(p[0]) {
		return false
	}
	return p[1] == "tool-results" || p[1] == "subagents" && !strings.HasSuffix(rel, ".jsonl")
}
func (e *Engine) homes(provider string) (codexhome.Home, claudehome.Home, error) {
	if provider == "claude" {
		c, err := claudehome.Detect(e.Config.ClaudeHome)
		if err != nil {
			return codexhome.Home{}, c, err
		}
		c, err = claudehome.Detect(canonicalPath(c.Root))
		if err != nil {
			return codexhome.Home{}, c, err
		}
		return codexhome.Home{Root: c.Root, SessionsDir: c.ProjectsDir}, c, nil
	}
	if provider != "codex" {
		return codexhome.Home{}, claudehome.Home{}, fmt.Errorf("仅支持 codex / claude")
	}
	h, err := codexhome.Detect(e.Config.CodexHome)
	if err == nil {
		h, err = codexhome.Detect(canonicalPath(h.Root))
	}
	return h, claudehome.Home{}, err
}
func (e *Engine) scan(provider string) (sessions.ScanResult, error) {
	h, c, err := e.homes(provider)
	if err != nil {
		return sessions.ScanResult{}, err
	}
	if provider == "claude" {
		return claudesessions.Scan(c, claudesessions.ScanOptions{})
	}
	return sessions.Scan(h, sessions.ScanOptions{IncludeArchived: true, DecompressCompressed: true})
}
func (e *Engine) Export(provider, key, out string) (Archive, error) {
	var m Archive
	e.report("check", "检查来源工具状态", 0, 0)
	if err := e.guard(provider); err != nil {
		return m, err
	}
	p, err := e.Config.project(key)
	if err != nil {
		return m, err
	}
	h, c, err := e.homes(provider)
	if err != nil {
		return m, err
	}
	before, err := treeStamp(h.SessionsDir)
	if err != nil {
		return m, err
	}
	archiveBefore, err := treeStamp(h.ArchivedSessionsDir)
	if err != nil {
		return m, err
	}
	if err = os.MkdirAll(e.Config.DataDir, 0700); err != nil {
		return m, err
	}
	tmp, err := os.MkdirTemp(e.Config.DataDir, "export-")
	if err != nil {
		return m, err
	}
	defer os.RemoveAll(tmp)
	native := filepath.Join(tmp, "sessions.codexbundle")
	e.report("compress", "正在无损压缩会话；大项目可能需要几分钟", 0, 0)
	res, err := bundle.Export(h, bundle.ExportOptions{Tool: agent.Kind(provider), ClaudeHome: c, ProjectPath: p.Path, OutputPath: native, IncludeArchived: true, WithMemory: provider == "claude"})
	if err != nil {
		return m, err
	}
	if res.CompressedSkipped > 0 {
		return m, fmt.Errorf("存在无法识别项目路径的压缩会话，请检查文件完整性与会话元数据")
	}
	if len(res.Warnings) > 0 {
		for _, w := range res.Warnings {
			if strings.Contains(w, "cannot ") || strings.Contains(w, "skip") {
				return m, fmt.Errorf("导出不完整：%s", w)
			}
		}
	}
	m = Archive{MinClientVersion: Version, Format: "session-relay-v1", Provider: provider, Project: key, SourcePath: p.Path, SourceOS: runtime.GOOS, Sessions: res.IncludedCount, Extras: []Extra{}}
	m.NativeSHA, err = hashFile(native)
	if err != nil {
		return m, err
	}
	for _, s := range res.Manifest.Sessions {
		if s.SizeBytes > maxFile {
			return m, fmt.Errorf("单个会话超过 2 GiB：%s", s.OriginalPath)
		}
		m.RawSize += s.SizeBytes
	}
	for _, x := range res.Manifest.Memory {
		m.RawSize += x.SizeBytes
	}
	extraFiles := map[string]string{}
	if provider == "claude" {
		projectRoot := filepath.Join(c.ProjectsDir, claudehome.EncodeCWD(p.Path))
		// Verify the selected project has no silently omitted JSONL, including children.
		included := map[string]bool{}
		for _, s := range res.Manifest.Sessions {
			included[filepath.Clean(s.OriginalPath)] = true
		}
		knownParents := map[string]bool{}
		for path := range included {
			if filepath.Dir(path) == projectRoot {
				knownParents[strings.TrimSuffix(filepath.Base(path), ".jsonl")] = true
			}
		}
		for parent := range knownParents {
			base := filepath.Join(projectRoot, parent)
			err = filepath.WalkDir(base, func(path string, d os.DirEntry, walkErr error) error {
				if os.IsNotExist(walkErr) {
					return nil
				}
				if walkErr != nil {
					return walkErr
				}
				if d.Type()&os.ModeSymlink != 0 {
					return fmt.Errorf("会话关联文件不能是符号链接：%s", path)
				}
				if d.IsDir() {
					return nil
				}
				rel, _ := filepath.Rel(projectRoot, path)
				rel = filepath.ToSlash(rel)
				if strings.HasSuffix(rel, ".jsonl") {
					if !included[filepath.Clean(path)] {
						return fmt.Errorf("子代理记录未纳入导出：%s", rel)
					}
					return nil
				}
				if !extraRelOK(rel) {
					return fmt.Errorf("未知关联文件类型，暂不猜测迁移：%s", rel)
				}
				st, err := d.Info()
				if err != nil {
					return err
				}
				if !st.Mode().IsRegular() || st.Size() > maxFile {
					return fmt.Errorf("不支持的关联文件：%s", rel)
				}
				sum, err := hashFile(path)
				if err != nil {
					return err
				}
				m.Extras = append(m.Extras, Extra{Rel: rel, Size: st.Size(), SHA256: sum})
				extraFiles[rel] = path
				m.RawSize += st.Size()
				return nil
			})
			if err != nil {
				return m, err
			}
		}
	}
	sort.Slice(m.Extras, func(i, j int) bool { return m.Extras[i].Rel < m.Extras[j].Rel })
	if m.RawSize > MaxExpanded {
		return m, fmt.Errorf("原始数据超过 8 GiB")
	}
	if err = os.MkdirAll(filepath.Dir(out), 0700); err != nil {
		return m, err
	}
	f, err := os.CreateTemp(filepath.Dir(out), ".relay-export-*")
	if err != nil {
		return m, err
	}
	defer os.Remove(f.Name())
	zw := zip.NewWriter(f)
	add := func(name, file string, method uint16) error {
		hdr := &zip.FileHeader{Name: name, Method: method}
		hdr.SetMode(0600)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		r, err := os.Open(file)
		if err != nil {
			return err
		}
		defer r.Close()
		_, err = io.Copy(w, r)
		return err
	}
	if err = add("sessions.codexbundle", native, zip.Store); err == nil {
		for _, x := range m.Extras {
			if err = add("extras/"+x.Rel, extraFiles[x.Rel], zip.Deflate); err != nil {
				break
			}
		}
	}
	if err == nil {
		w, x := zw.Create("relay.json")
		err = x
		if err == nil {
			err = json.NewEncoder(w).Encode(m)
		}
	}
	ze := zw.Close()
	ce := f.Close()
	if err != nil {
		return m, err
	}
	if ze != nil {
		return m, ze
	}
	if ce != nil {
		return m, ce
	}
	after, err := treeStamp(h.SessionsDir)
	if err != nil {
		return m, err
	}
	archiveAfter, err := treeStamp(h.ArchivedSessionsDir)
	if err != nil {
		return m, err
	}
	if before != after || archiveBefore != archiveAfter {
		return m, fmt.Errorf("导出时会话发生变化，请关闭工具后重试")
	}
	if _, err = InspectArchive(f.Name()); err != nil {
		return m, err
	}
	st, _ := os.Stat(f.Name())
	if st.Size() > MaxBundle {
		return m, fmt.Errorf("压缩包超过 2 GiB")
	}
	return m, os.Rename(f.Name(), out)
}
