package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ahmojo/codex-claude-transfer/internal/relay"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		fmt.Println("Session Relay " + relay.Version + "\n\n用法：\n  relay server --data ./server-data --listen 127.0.0.1:8787\n  relay client --config client.json\n  relay export --config client.json --provider codex --project my-app --out backup.relay.zip\n  relay preview --config client.json --project my-app --file backup.relay.zip\n  relay restore --config client.json --preview PREVIEW_ID\n  relay rollback --config client.json --restore RESTORE_ID\n  relay version")
		return nil
	}
	if os.Args[1] == "version" {
		fmt.Println(relay.Version)
		return nil
	}
	f := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
	config := f.String("config", "client.json", "客户端配置")
	data := f.String("data", "server-data", "服务器数据目录")
	listen := f.String("listen", "127.0.0.1:8787", "监听地址")
	public := f.String("public-url", "", "外部 HTTPS 地址")
	provider := f.String("provider", "codex", "codex 或 claude")
	project := f.String("project", "", "项目 key")
	out := f.String("out", "backup.relay.zip", "导出文件")
	file := f.String("file", "", "导入文件")
	preview := f.String("preview", "", "预览 ID")
	restore := f.String("restore", "", "恢复 ID")
	if err := f.Parse(os.Args[2:]); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if os.Args[1] == "server" {
		dir, err := filepath.Abs(*data)
		if err != nil {
			return err
		}
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		lock, err := relay.InstanceLock(dir)
		if err != nil {
			return err
		}
		defer lock.Close()
		token := strings.TrimSpace(os.Getenv("RELAY_ADMIN_TOKEN"))
		if token == "" {
			p := filepath.Join(dir, "admin-token.txt")
			b, err := os.ReadFile(p)
			if os.IsNotExist(err) {
				b = make([]byte, 0)
				secret := newSecret()
				if err = os.WriteFile(p, []byte(secret+"\n"), 0600); err != nil {
					return err
				}
				b = []byte(secret)
			} else if err != nil {
				return err
			}
			token = strings.TrimSpace(string(b))
			log.Printf("管理密钥保存在 %s（仅在网页登录使用）", p)
		}
		s, err := relay.NewServer(dir, *public, token)
		if err != nil {
			return err
		}
		go s.RunCleanup(ctx)
		srv := &http.Server{Addr: *listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 2 * time.Hour, WriteTimeout: 2 * time.Hour, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
		go func() {
			<-ctx.Done()
			c, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			srv.Shutdown(c)
		}()
		log.Printf("Session Relay %s http://%s", relay.Version, *listen)
		err = srv.ListenAndServe()
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
	if os.Args[1] == "setup-ssh" {
		return relay.ConfigureSSH(ctx, *config)
	}
	c, err := relay.LoadConfig(*config)
	if err != nil {
		return err
	}
	if os.Args[1] == "client" {
		client, err := relay.NewClient(c)
		if err != nil {
			return err
		}
		return client.Run(ctx)
	}
	lock, err := relay.InstanceLock(c.DataDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	engine := &relay.Engine{Config: c}
	inventory := engine.Inventory()
	var result any
	switch os.Args[1] {
	case "scan":
		result = map[string]any{"projects": engine.Config.Projects, "inventory": inventory}
	case "export":
		result, err = engine.Export(*provider, *project, *out)
	case "preview":
		result, err = engine.Preview(*file, *project, newSecret()[:32])
	case "restore":
		result, err = engine.Apply(*preview, newSecret()[:32])
	case "rollback":
		result, err = engine.Rollback(*restore)
	default:
		return fmt.Errorf("未知命令 %s", os.Args[1])
	}
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(b))
	return nil
}
