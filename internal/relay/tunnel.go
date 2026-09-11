package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

type SSHConfig struct {
	Host           string `json:"host"`
	User           string `json:"user"`
	Port           int    `json:"port,omitempty"`
	IdentityFile   string `json:"identity_file"`
	KnownHostsFile string `json:"known_hosts_file,omitempty"`
	RemotePort     int    `json:"remote_port,omitempty"`
}

func (c ClientConfig) sshArgs() ([]string, error) {
	s := c.SSH
	if s == nil {
		return nil, nil
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.:-]*$`).MatchString(s.Host) || !regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_-]*$`).MatchString(s.User) {
		return nil, fmt.Errorf("SSH 主机或用户名无效")
	}
	u, e := url.Parse(c.Server)
	if e != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
		return nil, fmt.Errorf("自动 SSH 的 server 必须是 http://127.0.0.1:端口")
	}
	port, e := strconv.Atoi(u.Port())
	if e != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("本地隧道端口无效")
	}
	sshPort, remote := s.Port, s.RemotePort
	if sshPort == 0 {
		sshPort = 22
	}
	if remote == 0 {
		remote = 8787
	}
	if sshPort < 1 || sshPort > 65535 || remote < 1 || remote > 65535 {
		return nil, fmt.Errorf("SSH 端口无效")
	}
	if s.IdentityFile == "" {
		return nil, fmt.Errorf("请先运行 setup-ssh.command / setup-ssh.cmd 配置 SSH 密钥")
	}
	key := expand(s.IdentityFile)
	if st, e := os.Stat(key); e != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("SSH 私钥文件不存在：%s", key)
	}
	known := s.KnownHostsFile
	if known == "" {
		known = "~/.ssh/known_hosts"
	}
	known = expand(known)
	return []string{"-N", "-T", "-p", strconv.Itoa(sshPort), "-i", key, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + known, "-o", "ExitOnForwardFailure=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-L", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + ":127.0.0.1:" + strconv.Itoa(remote), s.User + "@" + s.Host}, nil
}
func tunnelHealthy(ctx context.Context, server string) bool {
	cc, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(cc, "GET", server+"/healthz", nil)
	if e != nil {
		return false
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("redirect") }}
	defer client.CloseIdleConnections()
	res, e := client.Do(req)
	if e != nil {
		return false
	}
	defer res.Body.Close()
	var h struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	return res.StatusCode == 200 && json.NewDecoder(res.Body).Decode(&h) == nil && h.Status == "ok" && h.Version != ""
}

// Starts a supervised child, without an extra terminal window. Existing
// external tunnels are reused and are never killed by the client.
func (c *Client) startTunnel(ctx context.Context) (func(), error) {
	if c.Config.SSH == nil {
		return func() {}, nil
	}
	args, e := c.Config.sshArgs()
	if e != nil {
		return nil, e
	}
	exe, e := exec.LookPath("ssh")
	if e != nil {
		return nil, fmt.Errorf("未找到系统 OpenSSH，请先安装 OpenSSH 客户端")
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			if tunnelHealthy(ctx, c.Config.Server) {
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
				continue
			}
			log.Print("正在后台连接 SSH 隧道…")
			cmd := exec.CommandContext(ctx, exe, args...)
			hideSSHWindow(cmd)
			cmd.Stderr = os.Stderr
			err := cmd.Run()
			if ctx.Err() != nil {
				return
			}
			log.Printf("SSH 连接已断开，5 秒后重连：%v（首次使用请运行 setup-ssh）", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
	return func() { cancel(); <-done }, nil
}

// Resolve relative SSH paths against client.json, not the launching shell.
func resolveSSHPaths(c *ClientConfig, config string) {
	if c.SSH == nil {
		return
	}
	for _, p := range []*string{&c.SSH.IdentityFile, &c.SSH.KnownHostsFile} {
		if *p != "" && (*p)[0] != '~' && !filepath.IsAbs(*p) {
			*p = filepath.Join(filepath.Dir(expand(config)), *p)
		}
	}
}
