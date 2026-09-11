package relay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type Client struct {
	projectMu sync.RWMutex
	Config    ClientConfig
	Engine    *Engine
	HTTP      *http.Client
}
type Completion struct {
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

func NewClient(c ClientConfig) (*Client, error) {
	u, e := url.Parse(c.Server)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("服务器地址无效")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		local := u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()
		if u.Scheme != "http" || !local && !c.AllowHTTP {
			return nil, fmt.Errorf("远程连接要求 HTTPS；可信内网测试可在配置显式设置 allow_http:true")
		}
	}
	c.Server = strings.TrimRight(c.Server, "/")
	if e = os.MkdirAll(c.DataDir, 0700); e != nil {
		return nil, e
	}
	return &Client{Config: c, Engine: &Engine{Config: c}, HTTP: &http.Client{Timeout: 2 * time.Hour, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("服务器不应重定向 API 请求") }}}, nil
}
func (c *Client) request(ctx context.Context, method, path string, data io.Reader, contentType string) (*http.Response, error) {
	r, e := http.NewRequestWithContext(ctx, method, c.Config.Server+path, data)
	if e != nil {
		return nil, e
	}
	r.Header.Set("Authorization", "Bearer "+c.Config.Token)
	r.Header.Set("Content-Type", contentType)
	res, e := c.HTTP.Do(r)
	if e != nil {
		return nil, e
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		defer res.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(res.Body, 8192))
		return nil, fmt.Errorf("服务器返回 %d：%s", res.StatusCode, b)
	}
	return res, nil
}
func (c *Client) api(ctx context.Context, method, path string, in, out any) error {
	var data io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			return e
		}
		data = bytes.NewReader(b)
	}
	res, e := c.request(ctx, method, path, data, "application/json")
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if out != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(out)
	}
	_, e = io.Copy(io.Discard, res.Body)
	return e
}
func (c *Client) heartbeat(ctx context.Context, inv []Inventory) error {
	c.projectMu.RLock()
	projects := append([]Project{}, c.Config.Projects...)
	root := importRoot(c.Config)
	c.projectMu.RUnlock()
	return c.api(ctx, "POST", "/api/agent/heartbeat", map[string]any{"os": runtime.GOOS + "/" + runtime.GOARCH, "version": Version, "projects": projects, "inventory": inv, "import_root": root}, nil)
}
func (c *Client) download(ctx context.Context, sid string) (string, error) {
	if !validID.MatchString(sid) {
		return "", fmt.Errorf("快照 ID 无效")
	}
	dir := filepath.Join(c.Config.DataDir, "downloads")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return "", e
	}
	res, e := c.request(ctx, "GET", "/api/agent/bundles/"+sid, nil, "")
	if e != nil {
		return "", e
	}
	defer res.Body.Close()
	f, e := os.CreateTemp(dir, "download-*")
	if e != nil {
		return "", e
	}
	defer os.Remove(f.Name())
	meter := &progressReader{r: io.LimitReader(res.Body, MaxBundle+1), total: res.ContentLength, stage: "download", report: c.Engine.Progress}
	h, n, e := hashReader(io.TeeReader(meter, f))
	ce := f.Close()
	if e != nil {
		return "", e
	}
	if ce != nil {
		return "", ce
	}
	if n > MaxBundle || h != res.Header.Get("X-Content-SHA256") {
		return "", fmt.Errorf("下载大小或 SHA-256 校验失败")
	}
	dest := filepath.Join(dir, sid+".zip")
	if e = os.Rename(f.Name(), dest); e != nil {
		return "", e
	}
	return dest, nil
}
func (c *Client) execute(ctx context.Context, j Job) (any, error) {
	var progressMu sync.Mutex
	var last time.Time
	var lastStage string
	report := func(p Progress) {
		progressMu.Lock()
		defer progressMu.Unlock()
		if lastStage == p.Stage && time.Since(last) < time.Second && (p.Total == 0 || p.Done != p.Total) {
			return
		}
		last, lastStage = time.Now(), p.Stage
		cc, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_ = c.api(cc, "POST", "/api/agent/progress/"+j.ID, p, nil)
	}
	c.Engine.Progress = report
	defer func() { c.Engine.Progress = nil }()

	switch j.Kind {
	case "export":
		dir := filepath.Join(c.Config.DataDir, "exports")
		if e := os.MkdirAll(dir, 0700); e != nil {
			return nil, e
		}
		file := filepath.Join(dir, j.ID+".relay.zip")
		if _, e := c.Engine.Export(j.Provider, j.Project, file); e != nil {
			return nil, e
		}
		f, e := os.Open(file)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		stat, e := f.Stat()
		if e != nil {
			return nil, e
		}
		meter := &progressReader{r: f, total: stat.Size(), stage: "upload", report: report}
		report(Progress{Stage: "upload", Message: "正在上传到中转站", Total: stat.Size()})
		res, e := c.request(ctx, "POST", "/api/agent/upload/"+j.ID, meter, "application/zip")
		if e != nil {
			return nil, e
		}
		defer res.Body.Close()
		var b Snapshot
		if e = json.NewDecoder(res.Body).Decode(&b); e != nil {
			return nil, e
		}
		return map[string]any{"message": "已压缩并上传快照", "snapshot_id": b.ID, "size": b.Size, "raw_size": b.RawSize, "sessions": b.Sessions, "local_file": file}, nil
	case "preview":
		if providerOK(j.Provider) {
			if err := c.Engine.guard(j.Provider); err != nil {
				return nil, err
			}
		}
		report(Progress{Stage: "download", Message: "正在从中转站下载快照"})
		file, e := c.download(ctx, j.SnapshotID)
		if e != nil {
			return nil, e
		}
		if j.NewProject {
			meta, err := InspectArchive(file)
			if err != nil {
				return nil, err
			}
			if err = c.Engine.guard(meta.Provider); err != nil {
				return nil, err
			}
			if _, err = c.Engine.PrepareImport(j.Project, j.Folder); err != nil {
				return nil, err
			}
		}
		return c.Engine.Preview(file, j.Project, j.ID)
	case "scan":
		inv := c.refreshInventory()
		if err := c.heartbeat(ctx, inv); err != nil {
			return nil, err
		}
		return map[string]any{"message": fmt.Sprintf("扫描完成，发现 %d 个项目", len(c.Engine.Config.Projects)), "inventory": inv}, nil
	case "restore":
		return c.Engine.Apply(j.PreviewID, j.ID)
	case "rollback":
		return c.Engine.Rollback(j.RestoreID)
	default:
		return nil, fmt.Errorf("未知任务类型")
	}
}
func (c *Client) Run(ctx context.Context) error {
	lock, e := InstanceLock(c.Config.DataDir)
	if e != nil {
		return e
	}
	defer lock.Close()
	inv := c.refreshInventory()
	for {
		cc, cl := context.WithTimeout(ctx, 20*time.Second)
		e = c.heartbeat(cc, inv)
		cl()
		if e == nil {
			break
		}
		log.Printf("等待服务器连接：%v", e)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
	// Recover result delivery after disconnect without replaying a mutation.
	var current *Job
	if e = c.api(ctx, "GET", "/api/agent/current", nil, &current); e != nil {
		return e
	}
	if current != nil {
		done := Completion{}
		if readJSON(filepath.Join(c.Config.DataDir, "results", current.ID+".json"), &done) != nil {
			done.Error = "客户端曾中断，执行结果未知；不会重放。若为恢复任务，请使用该任务的回滚按钮检查并恢复。"
		}
		if e = c.api(ctx, "POST", "/api/agent/result/"+current.ID, done, nil); e != nil {
			return e
		}
	}
	log.Printf("Session Relay %s 已连接；设备 %s，项目 %d 个", Version, c.Config.DeviceID, len(c.Config.Projects))
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	lastInventory := time.Now()
	var pending *Job
	for {
		if ctx.Err() != nil {
			return nil
		}
		if pending != nil {
			var result Completion
			if e = readJSON(filepath.Join(c.Config.DataDir, "results", pending.ID+".json"), &result); e != nil {
				return e
			}
			if e = c.api(ctx, "POST", "/api/agent/result/"+pending.ID, result, nil); e == nil {
				pending = nil
			} else {
				log.Printf("结果回传暂未成功：%v", e)
			}
		}
		if time.Since(lastInventory) > 60*time.Second {
			inv = c.refreshInventory()
			lastInventory = time.Now()
		}
		hbCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		e = c.heartbeat(hbCtx, inv)
		cancel()
		if e != nil {
			log.Printf("连接暂不可用，稍后重试：%v", e)
		} else if pending == nil {
			var j *Job
			claimCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			e = c.api(claimCtx, "POST", "/api/agent/claim", map[string]any{}, &j)
			cancel()
			if e != nil {
				log.Printf("获取任务失败：%v", e)
			} else if j != nil {
				if !validID.MatchString(j.ID) {
					return fmt.Errorf("服务器返回非法任务 ID")
				}
				var existing Completion
				if readJSON(filepath.Join(c.Config.DataDir, "results", j.ID+".json"), &existing) == nil {
					pending = j
					continue // deliver the durable result; never execute it twice
				}
				// Heartbeats stay live during compression / transfer; jobs are still serialized.
				hbDone := make(chan struct{})
				go func(inventory []Inventory) {
					t := time.NewTicker(15 * time.Second)
					defer t.Stop()
					for {
						select {
						case <-hbDone:
							return
						case <-ctx.Done():
							return
						case <-t.C:
							cc, cl := context.WithTimeout(ctx, 10*time.Second)
							_ = c.heartbeat(cc, inventory)
							cl()
						}
					}
				}(append([]Inventory(nil), inv...))
				log.Printf("开始 %s / %s", j.Kind, j.ID)
				v, err := c.execute(ctx, *j)
				close(hbDone)
				result := Completion{}
				if err != nil {
					result.Error = err.Error()
					log.Printf("任务失败：%v", err)
				} else {
					result.Result, _ = json.Marshal(v)
					log.Printf("任务完成 %s", j.ID)
				}
				if e = writeJSON(filepath.Join(c.Config.DataDir, "results", j.ID+".json"), result); e != nil {
					return e
				}
				pending = j
				lastInventory = time.Time{}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
func InstanceLock(path string) (net.Listener, error) {
	a, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	h := sha256.Sum256([]byte(a))
	port := 30000 + (int(h[0])*256+int(h[1]))%20000
	l, e := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if e != nil {
		return nil, fmt.Errorf("该数据目录可能已有实例运行（锁端口 %d）：%w", port, e)
	}
	return l, nil
}

func (c *Client) refreshInventory() []Inventory {
	inv := c.Engine.Inventory()
	c.projectMu.Lock()
	c.Config.Projects = append([]Project{}, c.Engine.Config.Projects...)
	c.projectMu.Unlock()
	return inv
}

type progressReader struct {
	r           io.Reader
	total, done int64
	stage       string
	report      func(Progress)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.report != nil {
		total := p.total
		if total < 0 {
			total = 0
		}
		if total > 0 && total < p.done {
			total = p.done
		}
		message := "正在从中转站下载"
		if p.stage == "upload" {
			message = "正在上传到中转站"
		}
		p.report(Progress{Stage: p.stage, Message: message, Done: p.done, Total: total})
		if err == io.EOF && p.stage == "upload" {
			p.report(Progress{Stage: "verify-server", Message: "上传完成，服务器正在校验压缩包"})
		}
	}
	return n, err
}
