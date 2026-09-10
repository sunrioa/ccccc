package relay

import (
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webFiles embed.FS

type Server struct {
	Dir, PublicURL, Token string
	mu                    sync.Mutex
	state                 State
	uploadSlots           chan struct{}
	failures              map[string][]time.Time
}

func NewServer(dir, publicURL, token string) (*Server, error) {
	if len(token) < 32 {
		return nil, fmt.Errorf("管理密钥至少需要 32 个字符")
	}
	if publicURL != "" {
		u, e := url.Parse(publicURL)
		if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.Path != "" && u.Path != "/" {
			return nil, fmt.Errorf("public-url 必须是 http(s) 根地址")
		}
	}
	s := &Server{Dir: dir, PublicURL: strings.TrimRight(publicURL, "/"), Token: token, uploadSlots: make(chan struct{}, 2), failures: map[string][]time.Time{}}
	if e := os.MkdirAll(filepath.Join(dir, "bundles"), 0700); e != nil {
		return nil, e
	}
	s.state = State{Devices: map[string]*Device{}, Snapshots: map[string]*Snapshot{}, Jobs: map[string]*Job{}}
	if e := readJSON(filepath.Join(dir, "state.json"), &s.state); e != nil && !os.IsNotExist(e) {
		return nil, e
	}
	if s.state.Devices == nil || s.state.Jobs == nil || s.state.Snapshots == nil {
		return nil, fmt.Errorf("state.json 格式无效")
	}
	for _, j := range s.state.Jobs {
		if j.Status == "running" {
			j.Status = "failed"
			j.Error = "服务器曾中断，任务执行结果未知；先检查客户端日志和本地恢复记录，不会自动重复执行"
		}
	}
	return s, s.save()
}
func (s *Server) save() error { return writeJSON(filepath.Join(s.Dir, "state.json"), &s.state) }
func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, code int, e any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": fmt.Sprint(e)})
}
func body(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}
func equal(a, b string) bool { return hmac.Equal([]byte(a), []byte(b)) }
func (s *Server) cookieValue() string {
	exp := strconv.FormatInt(time.Now().Add(12*time.Hour).Unix(), 10)
	h := hmac.New(sha256.New, []byte(s.Token))
	h.Write([]byte(exp))
	return exp + "." + hex.EncodeToString(h.Sum(nil))
}
func (s *Server) isAdmin(r *http.Request) bool {
	if b := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); equal(b, s.Token) {
		return true
	}
	c, e := r.Cookie("relay_session")
	if e != nil {
		return false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 2 {
		return false
	}
	t, e := strconv.ParseInt(parts[0], 10, 64)
	if e != nil || t < time.Now().Unix() {
		return false
	}
	h := hmac.New(sha256.New, []byte(s.Token))
	h.Write([]byte(parts[0]))
	return equal(parts[1], hex.EncodeToString(h.Sum(nil)))
}
func (s *Server) deviceAuth(r *http.Request) string {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(tok) < 32 {
		return ""
	}
	h := hashBytes([]byte(tok))
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, d := range s.state.Devices {
		if equal(h, d.TokenHash) {
			return k
		}
	}
	return ""
}
func (s *Server) originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, e := url.Parse(o)
	if e != nil {
		return false
	}
	if s.PublicURL != "" {
		return o == s.PublicURL
	}
	return u.Host == r.Host && (u.Scheme == "http" || u.Scheme == "https")
}
func (s *Server) Handler() http.Handler {
	sub, _ := fs.Sub(webFiles, "web")
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if !s.originOK(r) {
			fail(w, 403, "不允许跨站请求")
			return
		}
		if r.URL.Path == "/healthz" {
			reply(w, map[string]string{"status": "ok", "version": Version})
			return
		}
		if r.URL.Path == "/api/login" && r.Method == "POST" {
			s.login(w, r)
			return
		}
		if r.URL.Path == "/api/logout" && r.Method == "POST" {
			http.SetCookie(w, &http.Cookie{Name: "relay_session", Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
			reply(w, map[string]bool{"ok": true})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/agent/") {
			d := s.deviceAuth(r)
			if d == "" {
				fail(w, 401, "设备密钥无效")
				return
			}
			s.agentAPI(w, r, d)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if !s.isAdmin(r) {
				fail(w, 401, "请先登录")
				return
			}
			s.adminAPI(w, r)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			fail(w, 405, "method not allowed")
			return
		}
		files.ServeHTTP(w, r)
	})
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.mu.Lock()
	now := time.Now()
	var recent []time.Time
	for _, t := range s.failures[host] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= 10 {
		s.mu.Unlock()
		fail(w, 429, "尝试过多，请一分钟后再试")
		return
	}
	recent = append(recent, now)
	if len(s.failures) > 10000 {
		s.failures = map[string][]time.Time{}
	}
	s.failures[host] = recent
	s.mu.Unlock()
	var b struct {
		Token string `json:"token"`
	}
	if e := body(w, r, &b); e != nil {
		fail(w, 400, e)
		return
	}
	if !equal(b.Token, s.Token) {
		fail(w, 401, "密钥不正确")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "relay_session", Value: s.cookieValue(), Path: "/", HttpOnly: true, Secure: strings.HasPrefix(s.PublicURL, "https://") || r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	reply(w, map[string]bool{"ok": true})
}
func (s *Server) adminAPI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/snapshots/upload" && r.Method == "POST" {
		s.receive(w, r, "manual", "")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/snapshots/") && r.Method == "GET" {
		sid := strings.TrimPrefix(r.URL.Path, "/api/snapshots/")
		s.download(w, r, sid)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.URL.Path == "/api/state" && r.Method == "GET":
		devices := []Device{}
		for _, d := range s.state.Devices {
			v := *d
			v.TokenHash = ""
			devices = append(devices, v)
		}
		sort.Slice(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })
		snapshots := []*Snapshot{}
		for _, b := range s.state.Snapshots {
			snapshots = append(snapshots, b)
		}
		sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Created.After(snapshots[j].Created) })
		jobs := []*Job{}
		for _, j := range s.state.Jobs {
			jobs = append(jobs, j)
		}
		sort.Slice(jobs, func(i, j int) bool { return jobs[i].Created.After(jobs[j].Created) })
		if len(jobs) > 100 {
			jobs = jobs[:100]
		}
		reply(w, map[string]any{"devices": devices, "snapshots": snapshots, "jobs": jobs, "version": Version, "public_url": s.PublicURL})
	case r.URL.Path == "/api/devices" && r.Method == "POST":
		var b struct {
			Name   string `json:"name"`
			Server string `json:"server"`
		}
		if e := body(w, r, &b); e != nil {
			fail(w, 400, e)
			return
		}
		if strings.TrimSpace(b.Name) == "" || len(b.Name) > 80 {
			fail(w, 400, "设备名称不能为空，最多 80 字节")
			return
		}
		base := s.PublicURL
		if base == "" {
			base = b.Server
		}
		u, e := url.Parse(base)
		if e != nil || u.Host == "" || u.Path != "" && u.Path != "/" || (u.Scheme != "http" && u.Scheme != "https") {
			fail(w, 400, "请提供服务器根地址")
			return
		}
		token := id() + id()
		did := id()
		s.state.Devices[did] = &Device{ID: did, Name: b.Name, TokenHash: hashBytes([]byte(token)), Projects: []Project{}, Inventory: []Inventory{}}
		if e = s.save(); e != nil {
			fail(w, 500, e)
			return
		}
		reply(w, ClientConfig{DeviceID: did, Token: token, Server: strings.TrimRight(base, "/"), Projects: []Project{}})
	case r.URL.Path == "/api/jobs" && r.Method == "POST":
		var j Job
		if e := body(w, r, &j); e != nil {
			fail(w, 400, e)
			return
		}
		d := s.state.Devices[j.DeviceID]
		if d == nil {
			fail(w, 400, "设备不存在")
			return
		}
		switch j.Kind {
		case "export":
			if !providerOK(j.Provider) || !hasProject(d, j.Project) {
				fail(w, 400, "工具或项目无效")
				return
			}
		case "scan":
			if d.ImportRoot == "" {
				fail(w, 400, "请先升级并连接 0.2 客户端")
				return
			}
		case "preview":
			b := s.state.Snapshots[j.SnapshotID]
			if j.NewProject {
				if d.ImportRoot == "" || validFolder(j.Folder) != nil {
					fail(w, 400, "设备不支持新建导入，或文件夹名称无效")
					return
				}
				j.Project = importKey(j.Folder)
			}
			if b == nil || !j.NewProject && !hasProject(d, j.Project) {
				fail(w, 400, "快照或目标项目无效")
				return
			}
			j.Provider = b.Provider
		case "restore":
			p := s.state.Jobs[j.PreviewID]
			if p == nil || p.Kind != "preview" || p.Status != "done" || p.DeviceID != j.DeviceID || s.state.Snapshots[p.SnapshotID] == nil {
				fail(w, 400, "必须先在该设备上预览")
				return
			}
			var plan Plan
			if json.Unmarshal(p.Result, &plan) != nil || !plan.CanApply {
				fail(w, 409, "预览存在冲突，不能恢复")
				return
			}
			j.SnapshotID = p.SnapshotID
			j.Project = p.Project
			j.Provider = p.Provider
		case "rollback":
			prev := s.state.Jobs[j.RestoreID]
			if prev == nil || prev.DeviceID != j.DeviceID || prev.Kind != "restore" || (prev.Status != "done" && prev.Status != "failed") {
				fail(w, 400, "恢复记录不存在")
				return
			}
			j.Provider = prev.Provider
			j.Project = prev.Project
		default:
			fail(w, 400, "未知任务类型")
			return
		}
		for _, old := range s.state.Jobs {
			if old.DeviceID == j.DeviceID && (old.Status == "queued" || old.Status == "running") {
				fail(w, 409, "该设备已有任务，请等待或取消排队任务")
				return
			}
		}
		j.ID = id()
		j.Status = "queued"
		j.Created = time.Now().UTC()
		j.Updated = j.Created
		j.Result = nil
		j.Error = ""
		s.state.Jobs[j.ID] = &j
		if e := s.save(); e != nil {
			fail(w, 500, e)
			return
		}
		reply(w, j)
	case strings.HasPrefix(r.URL.Path, "/api/jobs/") && r.Method == "DELETE":
		j := s.state.Jobs[strings.TrimPrefix(r.URL.Path, "/api/jobs/")]
		if j == nil || j.Status != "queued" {
			fail(w, 409, "只能取消尚未执行的任务")
			return
		}
		j.Status = "cancelled"
		j.Updated = time.Now().UTC()
		if e := s.save(); e != nil {
			fail(w, 500, e)
			return
		}
		reply(w, j)
	case strings.HasPrefix(r.URL.Path, "/api/snapshots/") && r.Method == "DELETE":
		sid := strings.TrimPrefix(r.URL.Path, "/api/snapshots/")
		b := s.state.Snapshots[sid]
		if b == nil {
			fail(w, 404, "快照不存在")
			return
		}
		for _, j := range s.state.Jobs {
			if j.SnapshotID == sid && (j.Status == "queued" || j.Status == "running") {
				fail(w, 409, "快照仍被排队或执行中的任务使用")
				return
			}
		}
		file := filepath.Join(s.Dir, "bundles", sid+".zip")
		if e := os.Rename(file, file+".deleted"); e != nil {
			fail(w, 500, e)
			return
		}
		delete(s.state.Snapshots, sid)
		if e := s.save(); e != nil {
			s.state.Snapshots[sid] = b
			_ = os.Rename(file+".deleted", file)
			fail(w, 500, e)
			return
		}
		_ = os.Remove(file + ".deleted")
		reply(w, map[string]bool{"ok": true})
	default:
		fail(w, 404, "not found")
	}
}
func hasProject(d *Device, key string) bool {
	for _, p := range d.Projects {
		if p.Key == key {
			return true
		}
	}
	return false
}
func (s *Server) agentAPI(w http.ResponseWriter, r *http.Request, did string) {
	if strings.HasPrefix(r.URL.Path, "/api/agent/bundles/") && r.Method == "GET" {
		s.download(w, r, strings.TrimPrefix(r.URL.Path, "/api/agent/bundles/"))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/agent/upload/") && r.Method == "POST" {
		s.receive(w, r, did, strings.TrimPrefix(r.URL.Path, "/api/agent/upload/"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.state.Devices[did]
	switch {
	case r.URL.Path == "/api/agent/current" && r.Method == "GET":
		for _, j := range s.state.Jobs {
			if j.DeviceID == did && j.Status == "running" {
				reply(w, j)
				return
			}
		}
		reply(w, nil)
	case r.URL.Path == "/api/agent/heartbeat" && r.Method == "POST":
		var b struct {
			OS         string      `json:"os"`
			Version    string      `json:"version"`
			ImportRoot string      `json:"import_root"`
			Projects   []Project   `json:"projects"`
			Inventory  []Inventory `json:"inventory"`
		}
		if e := body(w, r, &b); e != nil {
			fail(w, 400, e)
			return
		}
		if len(b.Projects) > 1000 || len(b.Inventory) > 2002 {
			fail(w, 400, "项目过多")
			return
		}
		d.OS = b.OS
		d.Version = b.Version
		d.ImportRoot = b.ImportRoot
		d.Projects = b.Projects
		d.Inventory = b.Inventory
		d.LastSeen = time.Now().UTC()
		if e := s.save(); e != nil {
			fail(w, 500, e)
			return
		}
		reply(w, map[string]bool{"ok": true})
	case r.URL.Path == "/api/agent/claim" && r.Method == "POST":
		var pick *Job
		for _, j := range s.state.Jobs {
			if j.DeviceID == did && j.Status == "running" {
				reply(w, j) // idempotent claim after a lost response
				return
			}
			if j.DeviceID == did && j.Status == "queued" && (pick == nil || j.Created.Before(pick.Created)) {
				pick = j
			}
		}
		if pick != nil {
			pick.Status = "running"
			pick.Updated = time.Now().UTC()
			if e := s.save(); e != nil {
				fail(w, 500, e)
				return
			}
		}
		reply(w, pick)
	case strings.HasPrefix(r.URL.Path, "/api/agent/result/") && r.Method == "POST":
		j := s.state.Jobs[strings.TrimPrefix(r.URL.Path, "/api/agent/result/")]
		if j == nil || j.DeviceID != did {
			fail(w, 404, "任务不存在")
			return
		}
		var b struct {
			Result json.RawMessage `json:"result"`
			Error  string          `json:"error"`
		}
		if e := body(w, r, &b); e != nil {
			fail(w, 400, e)
			return
		}
		if j.Status == "done" || j.Status == "failed" {
			reply(w, j)
			return
		}
		if j.Status != "running" {
			fail(w, 409, "任务状态已变化")
			return
		}
		j.Result = b.Result
		j.Error = b.Error
		j.Status = "done"
		if b.Error != "" {
			j.Status = "failed"
		}
		j.Updated = time.Now().UTC()
		if e := s.save(); e != nil {
			fail(w, 500, e)
			return
		}
		reply(w, j)
	default:
		fail(w, 404, "not found")
	}
}
func (s *Server) download(w http.ResponseWriter, r *http.Request, sid string) {
	s.mu.Lock()
	b := s.state.Snapshots[sid]
	s.mu.Unlock()
	if b == nil || !validID.MatchString(sid) {
		fail(w, 404, "快照不存在")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+b.Provider+"-"+sid+`.relay.zip"`)
	w.Header().Set("X-Content-SHA256", b.SHA256)
	http.ServeFile(w, r, filepath.Join(s.Dir, "bundles", sid+".zip"))
}
func (s *Server) receive(w http.ResponseWriter, r *http.Request, did, jid string) {
	select {
	case s.uploadSlots <- struct{}{}:
		defer func() { <-s.uploadSlots }()
	default:
		fail(w, 429, "上传繁忙，请稍后重试")
		return
	}
	s.mu.Lock()
	var total int64
	for _, b := range s.state.Snapshots {
		total += b.Size
	}
	j := s.state.Jobs[jid]
	allowed := did == "manual" || (j != nil && j.DeviceID == did && j.Kind == "export" && j.Status == "running")
	s.mu.Unlock()
	if !allowed {
		fail(w, 403, "上传任务无效")
		return
	}
	if total >= 20<<30 {
		fail(w, 507, "快照存储达到 20 GiB 上限，请先离线归档清理")
		return
	}
	f, e := os.CreateTemp(filepath.Join(s.Dir, "bundles"), "upload-*")
	if e != nil {
		fail(w, 500, e)
		return
	}
	defer os.Remove(f.Name())
	r.Body = http.MaxBytesReader(w, r.Body, MaxBundle)
	_, e = io.Copy(f, r.Body)
	ce := f.Close()
	if e != nil || ce != nil {
		fail(w, 400, "上传不完整或超过 1 GiB")
		return
	}
	m, e := InspectArchive(f.Name())
	if e != nil {
		fail(w, 400, e)
		return
	}
	if did != "manual" && (m.Provider != j.Provider || m.Project != j.Project) {
		fail(w, 400, "快照与任务不匹配")
		return
	}
	sum, e := hashFile(f.Name())
	if e != nil {
		fail(w, 500, e)
		return
	}
	stat, _ := os.Stat(f.Name())
	b := &Snapshot{ID: id(), DeviceID: did, Provider: m.Provider, Project: m.Project, SourcePath: m.SourcePath, SourceOS: m.SourceOS, Created: time.Now().UTC(), Size: stat.Size(), RawSize: m.RawSize, Sessions: m.Sessions, Extras: len(m.Extras), SHA256: sum}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, old := range s.state.Snapshots {
		if old.SHA256 == sum {
			reply(w, old)
			return
		}
	}
	if e = os.Rename(f.Name(), filepath.Join(s.Dir, "bundles", b.ID+".zip")); e != nil {
		fail(w, 500, e)
		return
	}
	s.state.Snapshots[b.ID] = b
	if e = s.save(); e != nil {
		fail(w, 500, e)
		return
	}
	reply(w, b)
}
