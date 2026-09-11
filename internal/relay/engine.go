package relay

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/ahmojo/codex-claude-transfer/internal/bundle"
	"github.com/ahmojo/codex-claude-transfer/internal/claudehome"
	"github.com/ahmojo/codex-claude-transfer/internal/codexreconcile"
)

var ErrAgentRunning = errors.New("对应 Agent 仍在运行")

type Engine struct {
	Progress      func(Progress)
	Config        ClientConfig
	guardOverride func(string) error
	skipDiscovery bool // only set by isolated tests; production performs native discovery.
}
type Journal struct {
	ID      string    `json:"id"`
	Plan    Plan      `json:"plan"`
	Status  string    `json:"status"`
	Created time.Time `json:"created"`
}

func hashReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, e := io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil)), n, e
}
func treeStamp(root string) (string, error) {
	if root == "" {
		return "", nil
	}
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("数据目录含符号链接：%s", p)
		}
		if d.IsDir() {
			return nil
		}
		s, e := d.Info()
		if e != nil {
			return e
		}
		fmt.Fprintf(h, "%s:%d:%d\n", p, s.Size(), s.ModTime().UnixNano())
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}
func (e *Engine) guard(provider string) error {
	if e.guardOverride != nil {
		return e.guardOverride(provider)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `$ErrorActionPreference='Stop'; Get-CimInstance Win32_Process | Select-Object Name,CommandLine | ConvertTo-Json -Compress`)
	} else {
		cmd = exec.CommandContext(ctx, "ps", "-A", "-o", "comm=", "-o", "args=")
	}
	b, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("无法检查 Agent 是否运行，暂不操作：%w", err)
	}
	text := strings.ToLower(string(b))
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("进程检查返回空结果，暂不操作")
	}
	if runtime.GOOS != "windows" {
		for _, line := range strings.Split(text, "\n") {
			fields := strings.Fields(line)
			if len(fields) > 0 && filepath.Base(fields[0]) == provider {
				return fmt.Errorf("%w：请先完全退出 %s，再点击重试；保留 relay 和 SSH 窗口", ErrAgentRunning, provider)
			}
		}
	}
	needles := []string{provider + ".exe", "/" + provider + " ", "/" + provider + "\n", "/" + provider + "\""}
	if provider == "claude" {
		needles = append(needles, "@anthropic-ai/claude-code", "claude.app/")
	} else {
		needles = append(needles, "@openai/codex", "codex.app/")
	}
	for _, n := range needles {
		if strings.Contains(text, n) {
			return fmt.Errorf("%w：请完全退出 %s 桌面应用和 CLI，再点击重试；保留 relay 和 SSH 窗口", ErrAgentRunning, provider)
		}
	}
	return nil
}
func (e *Engine) Preview(file, project, pid string) (Plan, error) {
	var plan Plan
	if !validID.MatchString(pid) {
		return plan, fmt.Errorf("预览 ID 无效")
	}
	e.report("verify", "正在校验压缩包", 0, 0)
	m, err := InspectArchive(file)
	if err != nil {
		return plan, err
	}
	p, err := e.Config.project(project)
	if err != nil {
		return plan, err
	}
	if st, x := os.Stat(p.Path); x != nil || !st.IsDir() {
		return plan, fmt.Errorf("目标项目目录不存在，请选择在本机新建项目：%s", p.Path)
	}
	h, _, err := e.homes(m.Provider)
	if err != nil {
		return plan, err
	}
	if err = e.guard(m.Provider); err != nil {
		return plan, err
	}
	if err = e.noPending(); err != nil {
		return plan, err
	}
	dir := filepath.Join(e.Config.DataDir, "previews", pid)
	if _, err = os.Stat(dir); err == nil {
		return plan, fmt.Errorf("预览已存在")
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return plan, err
	}
	success := false
	defer func() {
		if !success {
			os.RemoveAll(dir)
		}
	}()
	z, err := zip.OpenReader(file)
	if err != nil {
		return plan, err
	}
	defer z.Close()
	entries := map[string]*zip.File{}
	for _, f := range z.File {
		entries[f.Name] = f
	}
	native := filepath.Join(dir, "sessions.codexbundle")
	r, err := entries["sessions.codexbundle"].Open()
	if err != nil {
		return plan, err
	}
	nf, err := os.OpenFile(native, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		r.Close()
		return plan, err
	}
	_, err = io.Copy(nf, r)
	r.Close()
	ce := nf.Close()
	if err != nil {
		return plan, err
	}
	if ce != nil {
		return plan, ce
	}
	inspect, err := bundle.Inspect(native)
	if err != nil {
		return plan, err
	}
	if len(inspect.Manifest.Sessions)+len(inspect.Manifest.Memory)+len(m.Extras) > 5000 {
		return plan, fmt.Errorf("单次恢复超过 5000 个文件")
	}
	kind := inspect.Manifest.Tool
	if kind == "" {
		kind = "codex"
	}
	if kind != m.Provider || len(inspect.Manifest.Sessions) != m.Sessions {
		return plan, fmt.Errorf("原生包与外层工具类型或会话数量不一致")
	}
	for _, s := range inspect.Manifest.Sessions {
		if s.SizeBytes > maxFile {
			return plan, fmt.Errorf("单文件超过 2 GiB")
		}
		if s.OriginalCWD != "" && s.OriginalCWD != m.SourcePath {
			return plan, fmt.Errorf("快照混入其他项目：%s", s.OriginalCWD)
		}
	}
	for _, memory := range inspect.Manifest.Memory {
		if memory.ProjectCWD != m.SourcePath || memory.SizeBytes > maxFile {
			return plan, fmt.Errorf("记忆文件不属于该项目或大小超限")
		}
	}
	// Validate all inner entry names and expansion before upstream import planning.
	nz, err := zip.OpenReader(native)
	if err != nil {
		return plan, err
	}
	var total uint64
	seen := map[string]bool{}
	for _, f := range nz.File {
		if err = safeRel(f.Name); err != nil {
			break
		}
		key := strings.ToLower(f.Name)
		if seen[key] || !f.Mode().IsRegular() {
			err = fmt.Errorf("原生包存在重复或特殊文件")
			break
		}
		seen[key] = true
		total += f.UncompressedSize64
		if f.UncompressedSize64 > uint64(maxFile) || total > uint64(MaxExpanded) {
			err = fmt.Errorf("原生包展开超限")
			break
		}
	}
	nz.Close()
	if err != nil {
		return plan, err
	}
	res, expanded, err := bundle.PlanStreaming(h, bundle.ImportOptions{BundlePath: native, DryRun: true, IncludeArchived: true, MapCWD: []bundle.CWDMapping{{Old: m.SourcePath, New: p.Path}}, Merge: true, WithMemory: m.Provider == "claude"}, filepath.Join(dir, "stream"), func(done, total int) {
		e.report("plan", "正在映射路径、比较会话并生成预览", int64(done), int64(total))
	})
	if err != nil {
		return plan, err
	}
	for _, x := range m.Extras {
		expanded += x.Size
	}
	if expanded > MaxExpanded {
		return plan, fmt.Errorf("展开总量超过 8 GiB")
	}
	plan = Plan{ExpandedBytes: expanded, ID: pid, Provider: m.Provider, Project: project, Target: p.Path, CanApply: true, Changes: []Change{}, Warnings: append([]string{}, res.Warnings...), Created: time.Now().UTC()}
	plan.BundleSHA, err = hashFile(file)
	if err != nil {
		return plan, err
	}
	if res.MemoryConflicts > 0 || res.Conflicts > 0 || res.MappedCompressedSkipped > 0 || res.SkippedOther > 0 {
		plan.CanApply = false
	}
	destinations := map[string]bool{}
	stage := func(dest, action string, writePayload func(io.Writer) error) error {
		rel, x := filepath.Rel(h.Root, dest)
		if x != nil {
			return x
		}
		checked, x := securePath(h.Root, filepath.ToSlash(rel))
		if x != nil {
			return x
		}
		key := strings.ToLower(checked)
		if destinations[key] {
			return fmt.Errorf("多个记录指向同一目标：%s", checked)
		}
		destinations[key] = true
		before, x := fileHash(checked)
		if x != nil {
			return x
		}
		change := Change{Path: checked, Action: action, Before: before, After: before}
		if action == "conflict" {
			plan.CanApply = false
		}
		if writePayload != nil {
			staged := filepath.Join(dir, fmt.Sprintf("%06d.data", len(plan.Changes)))
			f, err := os.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			hash := sha256.New()
			err = writePayload(io.MultiWriter(f, hash))
			ce := f.Close()
			if err == nil {
				err = ce
			}
			if err != nil {
				return err
			}
			change.After = hex.EncodeToString(hash.Sum(nil))
		}
		plan.Changes = append(plan.Changes, change)
		return nil
	}
	for _, it := range res.Items {
		if it.DestPath == "" {
			if it.Action != bundle.ActionSkipNonSession {
				return plan, fmt.Errorf("原生包条目无法定位：%s", it.BundlePath)
			}
			continue
		}
		var payload func(io.Writer) error
		if it.Action == bundle.ActionImport || it.Action == bundle.ActionUpdate {
			payload = func(w io.Writer) error { return bundle.WritePlanned(native, it, w) }
		}
		if err = stage(it.DestPath, string(it.Action), payload); err != nil {
			return plan, err
		}
		if it.StagedPath != "" {
			_ = os.Remove(it.StagedPath)
		}
	}
	for _, x := range m.Extras {
		dest := filepath.Join(h.Root, "projects", claudehome.EncodeCWD(p.Path), filepath.FromSlash(x.Rel))
		before, err := fileHash(dest)
		if err != nil {
			return plan, err
		}
		action := "import"
		var payload func(io.Writer) error
		if before == x.SHA256 {
			action = "skip-identical"
		} else if before != "absent" {
			action = "conflict"
		} else {
			payload = func(w io.Writer) error {
				r, err := entries["extras/"+x.Rel].Open()
				if err != nil {
					return err
				}
				defer r.Close()
				_, err = io.Copy(w, r)
				return err
			}
		}
		if err = stage(dest, action, payload); err != nil {
			return plan, err
		}
	}
	if len(plan.Changes) > 5000 {
		return plan, fmt.Errorf("单次恢复超过 5000 个文件，请按更小项目范围迁移")
	}
	// Original transcript text stays intact. External paths in historical messages
	// are not rewritten globally, since that would alter user/tool content.
	if len(m.Extras) > 0 {
		plan.Warnings = append(plan.Warnings, "关联工具结果已纳入；历史消息中的旧绝对路径保留原文，可能需要在新设备手动定位对应文件。")
	}
	b, _ := json.Marshal(plan.Changes)
	plan.Fingerprint = hashBytes(b)
	if err = writeJSON(filepath.Join(dir, "plan.json"), plan); err != nil {
		return plan, err
	}
	success = true
	return plan, nil
}
func (e *Engine) noPending() error {
	entries, err := os.ReadDir(filepath.Join(e.Config.DataDir, "transactions"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, x := range entries {
		if !x.IsDir() {
			continue
		}
		var j Journal
		if err = readJSON(filepath.Join(e.Config.DataDir, "transactions", x.Name(), "journal.json"), &j); err != nil {
			return err
		}
		if j.Status != "done" && j.Status != "rolled_back" {
			return fmt.Errorf("存在未完成恢复 %s，请先执行回滚", j.ID)
		}
	}
	return nil
}
func (e *Engine) Apply(pid, jid string) (map[string]any, error) {
	if !validID.MatchString(pid) || !validID.MatchString(jid) {
		return nil, fmt.Errorf("无效 ID")
	}
	var p Plan
	dir := filepath.Join(e.Config.DataDir, "previews", pid)
	if err := readJSON(filepath.Join(dir, "plan.json"), &p); err != nil {
		return nil, err
	}
	if !p.CanApply {
		return nil, fmt.Errorf("预览存在冲突或未支持记录")
	}
	if time.Since(p.Created) > 24*time.Hour {
		return nil, fmt.Errorf("预览已超过 24 小时，请重新预览")
	}
	pr, err := e.Config.project(p.Project)
	if err != nil || pr.Path != p.Target {
		return nil, fmt.Errorf("目标项目配置变化，请重新预览")
	}
	if st, x := os.Stat(pr.Path); x != nil || !st.IsDir() {
		return nil, fmt.Errorf("目标项目目录已不存在，请重新预览")
	}
	if err = e.guard(p.Provider); err != nil {
		return nil, err
	}
	if err = e.noPending(); err != nil {
		return nil, err
	}
	h, _, err := e.homes(p.Provider)
	if err != nil {
		return nil, err
	}
	for i, c := range p.Changes {
		rel, err := filepath.Rel(h.Root, c.Path)
		if err != nil {
			return nil, err
		}
		if _, err = securePath(h.Root, filepath.ToSlash(rel)); err != nil {
			return nil, err
		}
		sum, err := fileHash(c.Path)
		if err != nil {
			return nil, err
		}
		if sum != c.Before {
			return nil, fmt.Errorf("预览后目标已变化，请重新预览：%s", c.Path)
		}
		if mutates(c) {
			sum, err = hashFile(filepath.Join(dir, fmt.Sprintf("%06d.data", i)))
			if err != nil || sum != c.After {
				return nil, fmt.Errorf("暂存数据校验失败")
			}
		}
	}
	td := filepath.Join(e.Config.DataDir, "transactions", jid)
	if _, err = os.Stat(td); !os.IsNotExist(err) {
		return nil, fmt.Errorf("恢复 ID 已存在，不能重复执行")
	}
	if err = os.MkdirAll(filepath.Dir(td), 0700); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(e.Config.DataDir, "transaction-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)
	j := Journal{ID: jid, Plan: p, Status: "prepared", Created: time.Now().UTC()}
	e.report("backup", "正在保存恢复前备份", 0, 0)
	for i, c := range p.Changes {
		if mutates(c) && c.Before != "absent" {
			if err = copyFile(c.Path, filepath.Join(staging, fmt.Sprintf("%06d.before", i))); err != nil {
				return nil, err
			}
		}
	}
	if err = writeJSON(filepath.Join(staging, "journal.json"), j); err != nil {
		return nil, err
	}
	if err = os.Rename(staging, td); err != nil {
		return nil, err
	}
	applyErr := func() error {
		for i, c := range p.Changes {
			e.report("restore", "正在恢复并校验会话", int64(i), int64(len(p.Changes)))
			if !mutates(c) {
				continue
			}
			sum, x := fileHash(c.Path)
			if x != nil || sum != c.Before {
				return fmt.Errorf("写入前文件发生变化：%s", c.Path)
			}
			if x = copyFile(filepath.Join(dir, fmt.Sprintf("%06d.data", i)), c.Path); x != nil {
				return x
			}
			sum, x = hashFile(c.Path)
			if x != nil || sum != c.After {
				return fmt.Errorf("写入后校验失败：%s", c.Path)
			}
		}
		return nil
	}()
	if applyErr != nil {
		_, rollbackErr := e.Rollback(jid)
		if rollbackErr != nil {
			return nil, fmt.Errorf("恢复失败：%v；自动回滚未完成：%v；备份位置：%s", applyErr, rollbackErr, td)
		}
		return nil, fmt.Errorf("恢复失败，已回滚：%w", applyErr)
	}
	j.Status = "done"
	if err = writeJSON(filepath.Join(td, "journal.json"), j); err != nil {
		return nil, err
	}
	result := map[string]any{"message": "文件恢复完成；请重新打开原工具确认历史并续聊。", "restore_id": jid, "backup_dir": td, "warnings": p.Warnings}
	if p.Provider == "codex" && !e.skipDiscovery {
		insp, x := bundle.Inspect(filepath.Join(dir, "sessions.codexbundle"))
		if x == nil {
			ids := []string{}
			for _, ss := range insp.Manifest.Sessions {
				if !ss.Archived {
					ids = append(ids, ss.ThreadID)
				}
			}
			rr, x := codexreconcile.Reconcile(context.Background(), codexreconcile.Options{CodexHome: h.Root, ThreadIDs: ids, Timeout: 30 * time.Second})
			if x != nil {
				result["discovery_warning"] = "会话文件已恢复，但 Codex 列表验证未完成：" + x.Error()
			} else {
				result["verified_threads"] = len(rr.Verified)
			}
		}
	}
	return result, nil
}
func mutates(c Change) bool { return c.Action == "import" || c.Action == "update" }
func (e *Engine) Rollback(jid string) (map[string]any, error) {
	if !validID.MatchString(jid) {
		return nil, fmt.Errorf("无效恢复 ID")
	}
	td := filepath.Join(e.Config.DataDir, "transactions", jid)
	var j Journal
	if err := readJSON(filepath.Join(td, "journal.json"), &j); err != nil {
		return nil, err
	}
	if j.Status == "rolled_back" {
		return map[string]any{"message": "已回滚"}, nil
	}
	if err := e.guard(j.Plan.Provider); err != nil {
		return nil, err
	}
	h, _, err := e.homes(j.Plan.Provider)
	if err != nil {
		return nil, err
	}
	for i, c := range j.Plan.Changes {
		if !mutates(c) {
			continue
		}
		rel, x := filepath.Rel(h.Root, c.Path)
		if x != nil {
			return nil, x
		}
		if _, x = securePath(h.Root, filepath.ToSlash(rel)); x != nil {
			return nil, x
		}
		now, x := fileHash(c.Path)
		if x != nil {
			return nil, x
		}
		if now != c.After && now != c.Before {
			return nil, fmt.Errorf("恢复后文件又有修改，拒绝覆盖：%s", c.Path)
		}
		if c.Before != "absent" {
			sum, x := hashFile(filepath.Join(td, fmt.Sprintf("%06d.before", i)))
			if x != nil || sum != c.Before {
				return nil, fmt.Errorf("备份校验失败：%s", c.Path)
			}
		}
	}
	for i, c := range j.Plan.Changes {
		if !mutates(c) {
			continue
		}
		if c.Before == "absent" {
			err = os.Remove(c.Path)
			if os.IsNotExist(err) {
				err = nil
			}
		} else {
			err = copyFile(filepath.Join(td, fmt.Sprintf("%06d.before", i)), c.Path)
		}
		if err != nil {
			return nil, err
		}
	}
	j.Status = "rolled_back"
	if err = writeJSON(filepath.Join(td, "journal.json"), j); err != nil {
		return nil, err
	}
	return map[string]any{"message": "已回滚会话文件；请重启原工具刷新索引。", "restore_id": jid}, nil
}
func SortedProjects(ps []Project) []Project {
	out := append([]Project{}, ps...)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (e *Engine) report(stage, message string, done, total int64) {
	if e.Progress != nil {
		e.Progress(Progress{Stage: stage, Message: message, Done: done, Total: total})
	}
}
