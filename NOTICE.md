# 来源与修改

Session Relay 的服务端、客户端、Web 界面、压缩容器和恢复事务层位于 `internal/relay`、`cmd/relay`。

会话解析、原生包校验与路径映射复用了 [ahmojo/codex-claude-transfer](https://github.com/ahmojo/codex-claude-transfer)，固定源提交：

`862fa52f0b279b556ad1655ec9e2b0472dd16307`

保留其模块路径以使用 internal 包；这不是上游官方发布。上游采用 MIT，完整声明在 `LICENSE.cct`。

本版本的兼容性修改：

1. Claude 子代理会话路径白名单支持标准 `session/subagents` 嵌套结构。
2. Claude 路径映射保留嵌套相对目录，避免子代理记录被移到项目根目录。
3. 没有 cwd 的子代理记录从确切父会话继承项目归属。
4. 原生导入暴露已校验的暂存内容，交由 Relay 事务层统一备份和回滚。
5. zstdcli 接口改为内置 `github.com/klauspost/compress/zstd v1.20.0`，不再依赖外部 zstd 程序；对应测试夹具改为内置编码器。

新增代码按 MIT 许可分发。压缩库的许可证随发布包提供。OpenAI / Anthropic 均未参与或认可本项目。

0.3 增加 `bundle.PlanStreaming`：保留旧 CCT API 的原限制与行为，Relay 改用经过完整校验的磁盘暂存、逐条 JSONL 映射和追加比较；避免扩大旧的整文件内存读取路径。

0.4 增加系统 OpenSSH 子进程监督、一次性主机信任设置、临时中转包生命周期以及不依赖服务器原包的已暂存预览恢复。
