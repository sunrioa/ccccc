package relay

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func ConfigureSSH(ctx context.Context, config string) error {
	c, e := LoadConfig(config)
	if e != nil {
		return e
	}
	if c.SSH == nil {
		c.SSH = &SSHConfig{User: "root", IdentityFile: "~/.ssh/id_ed25519", KnownHostsFile: "~/.ssh/known_hosts", Port: 22, RemotePort: 8787}
	}
	in := bufio.NewReader(os.Stdin)
	ask := func(label, def string) (string, error) {
		fmt.Printf("%s [%s]: ", label, def)
		line, e := in.ReadString('\n')
		if e != nil {
			return "", e
		}
		line = strings.TrimSpace(line)
		if line == "" {
			line = def
		}
		return line, nil
	}
	fmt.Println("一次性 SSH 设置：私钥留在本机；首次连接请核对服务器指纹。之后启动客户端会自动连接和重连。")
	if c.SSH.Host, e = ask("服务器 IP / 主机名", c.SSH.Host); e != nil {
		return e
	}
	if c.SSH.User, e = ask("SSH 用户", c.SSH.User); e != nil {
		return e
	}
	if c.SSH.IdentityFile, e = ask("SSH 私钥路径", c.SSH.IdentityFile); e != nil {
		return e
	}
	if c.SSH.KnownHostsFile, e = ask("主机指纹文件", c.SSH.KnownHostsFile); e != nil {
		return e
	}
	resolveSSHPaths(&c, config)
	args, e := c.sshArgs()
	if e != nil {
		return e
	}
	verify := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-N" {
			continue
		}
		if a == "-L" {
			i++
			continue
		}
		if a == "BatchMode=yes" {
			a = "BatchMode=no"
		}
		if a == "StrictHostKeyChecking=yes" {
			a = "StrictHostKeyChecking=ask"
		}
		verify = append(verify, a)
	}
	verify = append(verify, "exit")
	cmd := exec.CommandContext(ctx, "ssh", verify...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if e = cmd.Run(); e != nil {
		return fmt.Errorf("SSH 验证未通过，配置未保存：%w", e)
	}
	// Ensure unattended authentication works as well (encrypted keys need ssh-agent).
	for i := range verify {
		if verify[i] == "BatchMode=no" {
			verify[i] = "BatchMode=yes"
		}
		if verify[i] == "StrictHostKeyChecking=ask" {
			verify[i] = "StrictHostKeyChecking=yes"
		}
	}
	check := exec.CommandContext(ctx, "ssh", verify...)
	check.Stderr = os.Stderr
	if e = check.Run(); e != nil {
		return fmt.Errorf("SSH 尚不能免交互登录；请先将密钥加入 ssh-agent 后重试。配置未保存：%w", e)
	}
	var raw map[string]any
	if e = readJSON(config, &raw); e != nil {
		return e
	}
	raw["ssh"] = c.SSH
	if e = writeJSON(config, raw); e != nil {
		return e
	}
	fmt.Println("配置已保存。现在运行 start-client；无需再单独打开 SSH 窗口。")
	return nil
}
