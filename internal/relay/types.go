package relay

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const Version = "0.4.0"
const MaxBundle int64 = 2 << 30
const MaxExpanded int64 = 8 << 30

type Project struct {
	Key        string `json:"key"`
	Path       string `json:"path"`
	Discovered bool   `json:"discovered,omitempty"`
	Missing    bool   `json:"missing,omitempty"`
}
type ClientConfig struct {
	SSH             *SSHConfig `json:"ssh,omitempty"`
	Server          string     `json:"server"`
	DeviceID        string     `json:"device_id"`
	Token           string     `json:"token"`
	DataDir         string     `json:"data_dir"`
	CodexHome       string     `json:"codex_home,omitempty"`
	ClaudeHome      string     `json:"claude_home,omitempty"`
	Projects        []Project  `json:"projects"`
	ImportRoot      string     `json:"import_root,omitempty"`
	DisableAutoScan bool       `json:"disable_auto_scan,omitempty"`
	AllowHTTP       bool       `json:"allow_http,omitempty"`
}
type Inventory struct {
	Provider    string `json:"provider"`
	Project     string `json:"project"`
	Count       int    `json:"count"`
	Error       string `json:"error,omitempty"`
	Bytes       int64  `json:"bytes"`
	Largest     int64  `json:"largest"`
	AgentStatus string `json:"agent_status,omitempty"`
}
type Device struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	TokenHash  string      `json:"token_hash,omitempty"`
	OS         string      `json:"os"`
	Version    string      `json:"version"`
	ImportRoot string      `json:"import_root,omitempty"`
	LastSeen   time.Time   `json:"last_seen"`
	Projects   []Project   `json:"projects"`
	Inventory  []Inventory `json:"inventory"`
}
type Snapshot struct {
	Temporary        bool      `json:"temporary,omitempty"`
	Expires          time.Time `json:"expires,omitempty"`
	Purged           bool      `json:"purged,omitempty"`
	ID               string    `json:"id"`
	DeviceID         string    `json:"device_id"`
	Provider         string    `json:"provider"`
	Project          string    `json:"project"`
	SourcePath       string    `json:"source_path"`
	SourceOS         string    `json:"source_os"`
	Created          time.Time `json:"created"`
	Size             int64     `json:"size"`
	RawSize          int64     `json:"raw_size"`
	Sessions         int       `json:"sessions"`
	Extras           int       `json:"extras"`
	SHA256           string    `json:"sha256"`
	MinClientVersion string    `json:"min_client_version,omitempty"`
}
type Progress struct {
	Stage   string `json:"stage"`
	Message string `json:"message"`
	Done    int64  `json:"done"`
	Total   int64  `json:"total"`
}

type Job struct {
	KeepSnapshot bool            `json:"keep_snapshot,omitempty"`
	Progress     *Progress       `json:"progress,omitempty"`
	ID           string          `json:"id"`
	DeviceID     string          `json:"device_id"`
	Kind         string          `json:"kind"`
	Provider     string          `json:"provider,omitempty"`
	Project      string          `json:"project,omitempty"`
	SnapshotID   string          `json:"snapshot_id,omitempty"`
	NewProject   bool            `json:"new_project,omitempty"`
	Folder       string          `json:"folder,omitempty"`
	PreviewID    string          `json:"preview_id,omitempty"`
	RestoreID    string          `json:"restore_id,omitempty"`
	Status       string          `json:"status"`
	Created      time.Time       `json:"created"`
	Updated      time.Time       `json:"updated"`
	Result       json.RawMessage `json:"result,omitempty"`
	Error        string          `json:"error,omitempty"`
}
type State struct {
	Devices   map[string]*Device   `json:"devices"`
	Snapshots map[string]*Snapshot `json:"snapshots"`
	Jobs      map[string]*Job      `json:"jobs"`
}
type Change struct {
	Path   string `json:"path"`
	Action string `json:"action"`
	Before string `json:"before"`
	After  string `json:"after"`
}
type Plan struct {
	ID            string    `json:"id"`
	Provider      string    `json:"provider"`
	Project       string    `json:"project"`
	Target        string    `json:"target"`
	BundleSHA     string    `json:"bundle_sha"`
	Fingerprint   string    `json:"fingerprint"`
	ExpandedBytes int64     `json:"expanded_bytes,omitempty"`
	CanApply      bool      `json:"can_apply"`
	Changes       []Change  `json:"changes"`
	Warnings      []string  `json:"warnings"`
	Created       time.Time `json:"created"`
}

var validID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)

func id() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func hashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func hashFile(p string) (string, error) {
	f, e := os.Open(p)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	_, e = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), e
}
func fileHash(p string) (string, error) {
	h, e := hashFile(p)
	if errors.Is(e, os.ErrNotExist) {
		return "absent", nil
	}
	return h, e
}
func writeJSON(p string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return atomicWrite(p, b)
}
func atomicWrite(p string, b []byte) error {
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(p), ".relay-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(f.Name(), p)
}
func copyFile(src, dst string) error {
	f, e := os.Open(src)
	if e != nil {
		return e
	}
	defer f.Close()
	if e = os.MkdirAll(filepath.Dir(dst), 0700); e != nil {
		return e
	}
	out, e := os.CreateTemp(filepath.Dir(dst), ".relay-*")
	if e != nil {
		return e
	}
	defer os.Remove(out.Name())
	_, e = io.Copy(out, f)
	if e == nil {
		e = out.Sync()
	}
	ce := out.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(out.Name(), dst)
}
func readJSON(p string, v any) error {
	b, e := os.ReadFile(p)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func providerOK(p string) bool { return p == "codex" || p == "claude" }
func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		h, _ := os.UserHomeDir()
		p = filepath.Join(h, p[1:])
	}
	a, _ := filepath.Abs(p)
	return a
}

// Resolve existing ancestors as well, so a new /var/... home agrees with
// Codex's canonical /private/var/... spelling on macOS.
func canonicalPath(p string) string {
	cur := filepath.Clean(p)
	tail := []string{}
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				real = filepath.Join(real, tail[i])
			}
			return real
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}
func LoadConfig(p string) (ClientConfig, error) {
	var c ClientConfig
	if e := readJSON(p, &c); e != nil {
		return c, e
	}
	if !validID.MatchString(c.DeviceID) || len(c.Token) < 32 {
		return c, fmt.Errorf("device_id 或 token 无效")
	}
	if c.DataDir == "" {
		c.DataDir = filepath.Join(filepath.Dir(expand(p)), "relay-data-"+c.DeviceID)
	} else {
		c.DataDir = expand(c.DataDir)
	}
	if c.CodexHome != "" {
		c.CodexHome = expand(c.CodexHome)
	}
	if c.ClaudeHome != "" {
		c.ClaudeHome = expand(c.ClaudeHome)
	}
	resolveSSHPaths(&c, p)
	c.ImportRoot = importRoot(c)
	seen := map[string]bool{}
	for i := range c.Projects {
		pr := &c.Projects[i]
		if !validID.MatchString(pr.Key) || seen[pr.Key] || pr.Path == "" {
			return c, fmt.Errorf("projects 的 key 必须唯一，path 不可为空")
		}
		seen[pr.Key] = true
		pr.Path = expand(pr.Path)
	}
	return c, nil
}
func (c ClientConfig) project(key string) (Project, error) {
	for _, p := range c.Projects {
		if p.Key == key {
			return p, nil
		}
	}
	return Project{}, fmt.Errorf("未在客户端配置中允许项目 %s", key)
}
