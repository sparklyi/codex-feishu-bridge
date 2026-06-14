# codex-feishu-bridge

[English](README.md) | 简体中文

`codex-feishu-bridge` 是一个本地守护进程，让已授权的飞书用户可以通过飞书消息启动和继续 Codex 任务。它直接在用户自己的机器上运行 Codex，使用本机 Codex 登录态和本地文件系统权限，不依赖 Hermes 或 Codex Desktop。

## 快速开始

```bash
go install github.com/sparklyi/codex-feishu-bridge/cmd/codex-feishu-bridge@latest
codex-feishu-bridge init-config
```

编辑 `~/.codex-feishu-bridge/config.yaml`，然后运行：

```bash
export FEISHU_APP_SECRET=...
codex-feishu-bridge doctor
codex-feishu-bridge serve
```

## 飞书使用流程

发起一个新任务：

```text
/codex explain this repository
/codex @backend fix the failing test
```

守护进程会发送开始卡片，运行 `codex exec --json`，在本地保存 Codex thread id，然后发送结果卡片。回复开始卡片或结果卡片，或者提交卡片表单，可以通过 `codex exec --json resume` 继续同一个 Codex 会话。

## 常用命令

```bash
codex-feishu-bridge init-config [--config path] [--force]
codex-feishu-bridge doctor [--config path]
codex-feishu-bridge serve [--config path]
codex-feishu-bridge tasks list [--config path]
codex-feishu-bridge tasks show [--config path] <task_id>
```

## 配置文件

默认配置路径：

```text
~/.codex-feishu-bridge/config.yaml
```

示例配置见 [config.example.yaml](config.example.yaml)。飞书 app secret 不应写入配置文件，建议通过 `FEISHU_APP_SECRET` 等环境变量注入。

## 安全模型

只有 `security.allowed_open_ids` 中的飞书用户可以运行 Codex。未授权用户在私聊中会收到拒绝提示，在群聊中会被静默忽略。任务续写必须由任务创建者本人触发。

飞书卡片会隐藏本地绝对路径、secret、代理凭据和完整 Codex session id。原始 Codex JSONL 日志只保存在本机，默认位于 `~/.codex-feishu-bridge/logs`，文件权限为 `0600`。

## 本地权限

Codex 在本机执行，使用配置中的 workspace、sandbox、model 和 extra args。默认 sandbox 是 `workspace-write`；`danger-full-access` 不会由默认配置生成，必须显式配置。

## 更多文档

- [飞书配置](docs/feishu-setup.md)
- [安全模型](docs/security.md)
- [本地开发](docs/development.md)
- [故障排查](docs/troubleshooting.md)

## License

MIT
