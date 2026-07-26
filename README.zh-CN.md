# codex-feishu-bridge

[English](README.md) | 简体中文

`codex-feishu-bridge` 是一个供个人远程开发使用、仅接受可信飞书私聊指令的本地守护进程。它通过本机 Codex 的 `app-server` 标准输入输出协议与飞书连接，既能启动新任务，也能从飞书接管桌面 Codex 中已打开的会话；所有非私聊事件都会被忽略。

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

`doctor` 会启动本机 app-server 进程，并验证能否列出桌面 Codex 会话。`serve` 也会在开始接收飞书事件前完成同一项探测。

`app_server.command` 必须配置为通过官方安装器安装的独立 Codex CLI。不要填写 Codex Desktop 应用包内的可执行文件，独立 CLI 才提供本桥接所需的稳定 app-server 接口。

## 飞书使用流程

发起新任务：

```text
explain this repository
@backend fix the failing test
```

仅在配置了 `projects.backend` 时使用 `@backend`；省略前缀会使用 `workspace.default`。

在私聊中发送 `/sessions` 可以列出本机桌面 Codex 会话。选择一个会话后，桥接服务会创建绑定任务；在该任务卡片中继续输入内容，就会通过 app-server 恢复该会话。

运行中会始终使用同一张任务卡片，依据 app-server 的 item 事件展示当前阶段和关键里程碑，不展示推理、命令或命令输出。使用“补充到本轮”可向当前 Codex turn 追加约束，不会新建任务或卡片；停止操作会立即确认，不会等待 app-server 的中断请求完成。

任务结束后，原卡片会更新为包含结论、改动和验证的紧凑结果卡。点击“查看详情”会打开单独的分页卡片，展示最终回复全文。

`feishu.card_display_mode` 默认是 `concise`，只展示阶段与里程碑；设置为 `preview` 后，会低频展示回复草稿。两种模式都不会展示原始推理或工具输出。

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

网络需要代理时，在 `feishu.proxy_url` 填写 `http://` 代理地址；不配置时飞书 REST 与 WebSocket 均直连。桥接会忽略环境代理变量。

## 安全模型

只有 `security.allowed_open_ids` 中的飞书用户可以使用桥接服务。未授权用户在私聊中会收到拒绝提示，所有非私聊事件都会被忽略。继续任务和停止任务都必须由任务创建者本人触发。

任务卡片只会发送到私聊，并会隐藏本地绝对路径、secret、代理凭据和完整 Codex thread id。本机 SQLite 会在 `~/.codex-feishu-bridge/state.db` 保存任务与 Codex thread/turn 的关联。

## 本地权限

桥接服务只有一条执行链路：配置中的 `app_server` 命令和 app-server 协议。它只保存任务状态，不保存原始执行转录。每个 turn 都固定使用 `danger-full-access` 与 `approvalPolicy: never`，没有项目级权限覆盖，也不会再出现飞书审批卡。

## 更多文档

- [飞书机器人接入教程](docs/feishu-quickstart.zh-CN.md)
- [安全模型](docs/security.md)
- [本地开发](docs/development.md)
- [故障排查](docs/troubleshooting.md)

## License

MIT
