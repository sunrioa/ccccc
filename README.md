# Session Relay 0.2

Mac / Windows 的 Codex 与 Claude Code 会话，通过自己的 Linux 服务器中转。

这是一份可试用的 MVP：自动发现项目、网页管理、压缩快照、跨机器项目路径映射、恢复预览、增量追加、冲突阻止和本地回滚。仅支持同工具迁移。

## 运行结构

Mac 客户端 → HTTPS → Linux 中转站 ← HTTPS ← Windows 客户端。

Linux 保存压缩包和任务队列，提供 Web 管理端；Mac 和 Windows 主动轮询服务器。来源电脑上传完成后可以关机，目标电脑之后再取回。服务器不运行 Codex / Claude，也不需要你的模型账号。

## 1. 启动 Linux 中转站

下载对应 Linux CPU 架构的发布包，解压后：

```sh
chmod +x relay
./relay server --listen 127.0.0.1:8787 --data ./server-data
```

首次启动自动生成 `server-data/admin-token.txt`。这是网页登录密钥，不是模型 API Key。也可通过环境变量 `RELAY_ADMIN_TOKEN` 指定至少 32 字符的随机密钥。

**最快试用：通过 SSH 隧道访问，不需要先配置域名。** 在自己的电脑运行：

```sh
ssh -N -L 8787:127.0.0.1:8787 你的用户名@服务器地址
```

打开 `http://127.0.0.1:8787`，输入服务器上的管理密钥。通过该隧道工作的客户端配置 `server` 同样填写 `http://127.0.0.1:8787`。Mac / Windows 各自建立隧道；访问期间保持 SSH 开启。

长期使用建议配置 HTTPS 域名。`deploy/Caddyfile.example` 和 `deploy/session-relay.service` 提供反向代理与 systemd 模板。服务可使用：

```sh
./relay server --listen 127.0.0.1:8787 --data ./server-data --public-url https://sessions.example.com
```

`public-url` 必须与实际浏览器访问的根地址一致。不要把域名例子原样使用。没有配置 HTTPS 时，不要把管理接口直接暴露到公网。

## 2. 连接 Mac 和 Windows

1. 在网页「我的设备 → 添加设备」分别为两台电脑生成配置。
2. 无需填写项目。客户端启动时自动扫描 Codex 和 Claude Code 的会话存储，并约每分钟刷新项目列表；也可在网页点击「立即扫描项目」。
3. 下载 `client.json`，放在各自发布包内 `relay` / `relay.exe` 旁。
4. 启动客户端，并保持程序开启：

Mac：

```sh
chmod +x relay
./relay client --config client.json
```

Windows PowerShell：

```powershell
.\relay.exe client --config client.json
```

也可使用发布包附带的启动脚本。客户端无需安装 Go、Python、Node、rsync 或 zstd。Windows 进程检查使用系统 PowerShell；Mac/Linux 使用系统 ps。安装的原工具仍然是继续对话所必需的。

默认自动发现 Codex 和 Claude 的本地存储及其中所有项目，无需手写 `projects`，可以设为 `[]`。同一目录的两种工具会归到同一个项目；同名但路径不同的项目分开显示。原代码目录已不存在时，仍可扫描并导出保留的历史。自动扫描只上报项目路径、记录数量与扫描提示，创建快照后才上传会话正文。

默认的新项目导入目录为用户目录下的 `SessionRelayProjects`，可通过 `import_root` 改为自己的位置。`disable_auto_scan: true` 可关闭自动发现，仅使用明确配置和已保存的映射。客户端本机的 `project-mappings.json` 保存新建项目映射，重启后会自动恢复。

自定义存储目录可以填写 `codex_home` / `claude_home`。编辑配置后重启客户端。

```json
{
  "server": "https://sessions.example.com",
  "device_id": "网页生成的设备ID",
  "token": "网页生成的设备密钥",
  "projects": [
    {"key": "my-app", "path": "D:\\code\\my-app"}
  ]
}
```

Mac 的 path 类似 `/Users/你的用户名/code/my-app`。可恢复到自动发现的本机项目，也可选择「在本机新建项目」，无需本机已有对应会话。项目代码和工作环境仍需自行准备。

## 3. 日常使用

1. 完全退出来源电脑上的对应 Agent（桌面版与 CLI），点击「创建快照」。
2. 选择来源设备、项目和工具，等待压缩上传完成。
3. 快照库里点击「恢复到设备」，选择目标电脑。可选择已有本地项目，或选择「在本机新建项目」。后者自动给出文件夹名称，也可以修改。Mac 上没有但 Windows 有的项目，以及反向场景，均用此方式导入。
4. 目标电脑同样关闭对应 Agent。到「传输与恢复」查看预览。
5. 新建项目的预览会创建空目录和映射；此时还未写入会话文件。无冲突时点击「确认恢复」。系统再次检查目标文件，先备份、再写入、最后核对 SHA-256。
6. 重启原工具，确认历史并继续对话。Codex 会尝试调用本机 app-server 验证列表可见性；失败会单独提示，不会把文件恢复失败与列表验证失败混为一谈。

在 Windows 增加新对话后，用同样的流程上传，再恢复回 Mac。只有线性增长才会追加；真正分叉会阻止整次恢复，服务器快照与本机文件都保留。

项目列表自动扫描；快照上传和恢复仍需用户触发。当前版本为**手动触发的快照同步**，不是实时双向镜像。不自动同步删除，不自动合并分叉，不后台自动覆盖会话。

## 压缩包与恢复

网页可下载 `.relay.zip` 文件，也能将下载的包重新导入快照库。压缩是无损 ZIP；原生 `.jsonl.zst` 的解压与重压缩已经内置。不会摘要对话、删图片或删工具输出来减小体积。

命令行也可离线使用（先停止使用同一配置的常驻客户端）：

```sh
./relay scan --config client.json
./relay export --config client.json --provider codex --project my-app --out backup.relay.zip
./relay preview --config client.json --project my-app --file backup.relay.zip
./relay restore --config client.json --preview 上一步返回的id
./relay rollback --config client.json --restore 恢复记录的id
```

Claude 把 `--provider` 改为 `claude`。预览有效期为 24 小时；目标发生变化会要求重新预览。

新建导入产生的空项目目录和映射在取消预览或回滚后保留，不会删除其中可能已经放入的代码。

每次恢复记录保存在客户端数据目录的 `transactions/<恢复ID>/`。网页「撤销本次恢复」可回滚。若恢复后又产生新内容，回滚会拒绝覆盖，保留备份供人工处理。中途中断的恢复会阻止继续写入，必须先回滚。

## 范围与限制

- Codex：原生 rollout、归档 rollout、内联图片及工具记录；恢复后尽力注册到原生会话列表。
- Claude：主 JSONL、标准 `session/subagents/*.jsonl`、`tool-results`、子代理辅助文件，以及项目 `memory/`。
- Claude 子代理目录结构在路径映射后保留；缺少 cwd 的子代理只从其确切父会话继承项目归属。
- 历史消息原文中的旧绝对路径不做全局字符串替换。外置工具结果会复制到新存储位置，但旧路径文字可能需要手动定位。外部任意项目附件不会自动打包。
- 不包含账号凭据、登录状态、整个项目代码、运行中的进程状态或完整 Agent 配置；不是 ChatGPT / Claude 网页聊天同步。
- 对未知关联文件或无法定位的子代理会报错，避免静默丢弃。不保证所有未来版本的内部格式都兼容。
- 首版限制：每个原生文件/解压后的 zstd 会话最多 128 MiB，每个上传包 1 GiB，展开总量 4 GiB，每次恢复最多 5000 个文件；服务器快照存储约 20 GiB 上限。
- 快照删除只清理服务器对应压缩包。本地导出、下载、暂存和备份不自动清理；确认不再需要恢复后可手动归档客户端数据目录。
- 原始会话以文件形式存放在自己的服务器，**没有端到端加密或静态加密**；传输应使用 HTTPS 或 SSH 隧道。各设备可读取该私人中转站全部快照，设备密钥不能访问管理 API。
- 单用户、单服务进程设计，不适合多人租户或多个服务实例共用数据目录。
- Windows 二进制经过交叉编译，但尚未在 Windows 实机上完成真实客户端续聊验收。Claude 的真实客户端续聊也尚未实测。具体测试结果见 `VALIDATION.md`。

## Linux 常驻服务（可选）

以管理员身份创建专用用户、目录并安装二进制；以下以已经解压的 Linux 发布包为当前目录：

```sh
sudo useradd --system --home /var/lib/session-relay --shell /usr/sbin/nologin session-relay
sudo install -d -o session-relay -g session-relay /var/lib/session-relay
sudo install -d /opt/session-relay
sudo install -m 755 relay /opt/session-relay/relay
sudo install -m 644 deploy/session-relay.service /etc/systemd/system/session-relay.service
sudo systemctl daemon-reload
sudo systemctl enable --now session-relay
sudo cat /var/lib/session-relay/admin-token.txt
```

如用户已存在，略过 useradd。需要指定域名时，在 unit 的 ExecStart 后增加 `--public-url https://你的域名`，再 daemon-reload 并重启。Caddy 单独安装和运行。服务器不需要安装任何 Agent。

## 开发

Go 1.25 或更新版本：

```sh
go test ./...
go build -o relay ./cmd/relay
python3 scripts/release.py --out dist
```

构建脚本生成 macOS、Windows、Linux 的 amd64 / arm64 发布包。测试只使用隔离目录，不读取真实会话。额外的实际 HTTP / Codex 读取验证：

```sh
RELAY_NETWORK_TEST=1 go test -race ./internal/relay -run TestClientPollingOverRealHTTP -v
RELAY_NATIVE_TEST=1 go test ./internal/relay -run TestNativeCodex -v
```

第二条要求本机已有 Codex CLI；只验证隔离测试记录的读取和列表注册，不发送模型请求。

代码来源、许可证及兼容性修改见 `NOTICE.md`。

## 从 0.1 升级

停止服务端和客户端，用 0.2 程序替换原程序后重新启动。保留原 `client.json`、服务器数据和客户端数据目录，旧快照继续兼容。已有手工项目配置会保留，并额外自动发现其他项目。新建导入要求服务端和目标客户端都更新到 0.2。
