package relay

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ahmojo/codex-claude-transfer/internal/sessions"
)

func importRoot(c ClientConfig) string {
	if c.ImportRoot != "" {
		return expand(c.ImportRoot)
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, "SessionRelayProjects")
}
func pathKey(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}
func validFolder(folder string) error {
	if folder == "" || len(folder) > 120 || strings.ContainsAny(folder, "/\\:") || folder == "." || folder == ".." {
		return fmt.Errorf("请使用单个文件夹名称，不能含路径分隔符")
	}
	return safeRel(folder)
}
func importKey(folder string) string {
	return "import-" + hashBytes([]byte(strings.ToLower(folder)))[:16]
}
func autoKey(path string) string {
	name := filepath.Base(path)
	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	name = b.String()
	if name == "" {
		name = "project"
	}
	if len(name) > 40 {
		name = name[:40]
	}
	return name + "-" + hashBytes([]byte(pathKey(path)))[:12]
}
func (e *Engine) loadMappings() error {
	var ps []Project
	err := readJSON(filepath.Join(e.Config.DataDir, "project-mappings.json"), &ps)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, p := range ps {
		if !validID.MatchString(p.Key) || !filepath.IsAbs(p.Path) {
			return fmt.Errorf("本机项目映射无效")
		}
		found := false
		for _, current := range e.Config.Projects {
			if current.Key == p.Key {
				if pathKey(current.Path) != pathKey(p.Path) {
					return fmt.Errorf("项目映射与配置冲突：%s", p.Key)
				}
				found = true
			}
		}
		if !found {
			e.Config.Projects = append(e.Config.Projects, p)
		}
	}
	return nil
}

// Scans only the two tools' native session stores, never the whole disk.
func (e *Engine) Inventory() []Inventory {
	out := []Inventory{}
	if err := e.loadMappings(); err != nil {
		out = append(out, Inventory{Error: "读取项目映射失败：" + err.Error()})
	}
	scans := map[string]sessions.ScanResult{}
	errs := map[string]error{}
	known := map[string]bool{}
	keys := map[string]bool{}
	for _, p := range e.Config.Projects {
		known[pathKey(p.Path)] = true
		keys[p.Key] = true
	}
	for _, provider := range []string{"codex", "claude"} {
		scan, err := e.scan(provider)
		scans[provider], errs[provider] = scan, err
		if !e.Config.DisableAutoScan {
			for _, s := range scan.Sessions {
				if s.CWD == "" || !filepath.IsAbs(s.CWD) || known[pathKey(s.CWD)] {
					continue
				}
				if len(e.Config.Projects) >= 1000 {
					out = append(out, Inventory{Provider: provider, Error: "项目数已达 1000 上限，请缩小存储范围"})
					break
				}
				key := autoKey(s.CWD)
				if keys[key] {
					continue
				}
				e.Config.Projects = append(e.Config.Projects, Project{Key: key, Path: filepath.Clean(s.CWD), Discovered: true})
				known[pathKey(s.CWD)], keys[key] = true, true
			}
		}
	}
	e.Config.Projects = SortedProjects(e.Config.Projects)
	for i := range e.Config.Projects {
		st, err := os.Stat(e.Config.Projects[i].Path)
		e.Config.Projects[i].Missing = err != nil || !st.IsDir()
	}
	for _, provider := range []string{"codex", "claude"} {
		scan, err := scans[provider], errs[provider]
		agentStatus := "ready"
		if guardErr := e.guard(provider); errors.Is(guardErr, ErrAgentRunning) {
			agentStatus = "running"
		} else if guardErr != nil {
			agentStatus = "unknown"
		}
		if len(e.Config.Projects) == 0 && (err != nil || len(scan.Warnings) > 0) {
			x := Inventory{Provider: provider, Error: strings.Join(scan.Warnings, "; ")}
			if err != nil {
				x.Error = err.Error()
			}
			out = append(out, x)
		}
		for _, p := range e.Config.Projects {
			x := Inventory{Provider: provider, Project: p.Key, AgentStatus: agentStatus}
			if err != nil {
				x.Error = err.Error()
			} else {
				for _, s := range scan.Sessions {
					if pathKey(s.CWD) == pathKey(p.Path) {
						x.Count++
						x.Bytes += s.SizeBytes
						if s.SizeBytes > x.Largest {
							x.Largest = s.SizeBytes
						}
					}
				}
				x.Error = strings.Join(scan.Warnings, "; ")
			}
			out = append(out, x)
		}
	}
	return out
}

// Creates only an empty folder and a durable local mapping. Session files are
// still staged by Preview and written only by Apply. Empty folders remain on rollback.
func (e *Engine) PrepareImport(key, folder string) (Project, error) {
	var p Project
	if err := validFolder(folder); err != nil {
		return p, err
	}
	if key != importKey(folder) {
		return p, fmt.Errorf("新建项目标识不匹配")
	}
	if err := e.loadMappings(); err != nil {
		return p, err
	}
	root := canonicalPath(importRoot(e.Config))
	if err := os.MkdirAll(root, 0700); err != nil {
		return p, err
	}
	dest := filepath.Join(root, folder)
	if st, err := os.Lstat(dest); err == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return p, fmt.Errorf("导入路径不是普通目录")
		}
	} else if !os.IsNotExist(err) {
		return p, err
	}
	for _, current := range e.Config.Projects {
		if current.Key == key && pathKey(current.Path) != pathKey(dest) {
			return p, fmt.Errorf("项目标识已用于其他路径")
		}
		if pathKey(current.Path) == pathKey(dest) && current.Key != key {
			return p, fmt.Errorf("此目录已属于项目 %s，请从已有项目中选择", current.Key)
		}
	}
	p = Project{Key: key, Path: dest}
	var saved []Project
	err := readJSON(filepath.Join(e.Config.DataDir, "project-mappings.json"), &saved)
	if err != nil && !os.IsNotExist(err) {
		return p, err
	}
	found := false
	for _, existing := range saved {
		if existing.Key == key {
			found = true
		}
	}
	if !found {
		saved = append(saved, p)
	}
	if err = os.MkdirAll(dest, 0700); err != nil {
		return p, err
	}
	if err = writeJSON(filepath.Join(e.Config.DataDir, "project-mappings.json"), saved); err != nil {
		return p, err
	}
	found = false
	for _, existing := range e.Config.Projects {
		if existing.Key == key {
			found = true
		}
	}
	if !found {
		e.Config.Projects = append(e.Config.Projects, p)
	}
	return p, nil
}
