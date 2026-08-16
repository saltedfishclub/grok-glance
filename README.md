# grok-glance

给 [grok](https://github.com/xai-org/grok-build) 终端会话加一个浏览器遥控台：镜像正在跑的 TUI，从网页里发 prompt、打断 turn、批准工具调用。终端不会交出控制权，两边同时可用。

一个 Go 二进制，前端编进去，一个端口，没有数据库。

```
你的机器                              任何能打开浏览器的地方
┌─────────────────────────┐           ┌──────────────────┐
│  grok TUI（newgrok）    │           │                  │
│         │               │           │                  │
│         └── /rc ══ WSS ═╪══▶ glance ══ WSS ══▶ 浏览器  │
└─────────────────────────┘           └──────────────────┘
```

grok 主动连出来。glance 可以跑在你能访问的地方，跑 grok 的那台机器不必开端口、不必做内网穿透。

## 必须搭配 newgrok

glance 自己不会跑 agent。`/rc` 桥在 [iceBear67/newgrok](https://github.com/iceBear67/newgrok) 里：那是 `xai-org/grok-build` 的补丁工作流，远程控制是其中的 `0003` / `0004` 两个补丁。

**这两边是同一个功能。** 官方上游没有 `/rc`，单独用本仓库或单独用未打补丁的 grok 都连不上。线格式改一边，另一边也要改。

| 仓库 | 职责 |
|---|---|
| [iceBear67/newgrok](https://github.com/iceBear67/newgrok) | 打过补丁的 grok TUI；在会话里敲 `/rc`，作为 ACP Agent 拨出 |
| 本仓库 | 自托管控制面：鉴权、会话列表、把 ACP 转成网页 UI |

## 依赖

- Go 1.24+
- Node.js（构建嵌入的前端；`make build` 会跑 `npm`）
- 一台已经按 [newgrok 的 README](https://github.com/iceBear67/newgrok#快速开始) 编译好的 grok（`make setup && make apply && make build`）

## 快速开始

### 1. 编译并启动 glance

```sh
make build
bin/glance serve --addr 127.0.0.1:7717 --insecure-cookie
```

首次启动会在 stderr 打出一条一次性 bootstrap URL（同时写到 `~/.grok/glance/bootstrap.token`）。打开它，扫二维码，输入一个 TOTP 验证码。登记成功后这个 token 立刻作废。

`--insecure-cookie` 去掉 cookie 的 `Secure`，好让纯 HTTP 的 localhost 能登录。非回环地址会直接拒绝这个 flag。生产环境把 glance 放在终止 TLS 的反代后面，不要加这个 flag。

### 2. 给这台 grok 发一把 API key

另开一个终端：

```sh
bin/glance apikey add laptop
```

明文 `glance_sk_…` 只显示这一次，磁盘上只留 SHA-256。命令还会打出一段给 `~/.grok/config.toml` 用的配置。

### 3. 在跑 grok 的机器上写配置

把上一步打印的内容写进 `~/.grok/config.toml`：

```toml
[remote_control]
url     = "ws://127.0.0.1:7717/api/acp/agent"
api_key = "glance_sk_…"
```

glance 不在本机时，把 `url` 换成它的地址（TLS 后面用 `wss://…`）。key 也可以不写进文件，改用环境变量：

| 配置项 | 环境变量 | 说明 |
|---|---|---|
| `url` | `GROK_RC_URL` | glance 的 agent WebSocket |
| `api_key` | `GROK_RC_API_KEY` | `glance apikey add` 打出来的那把 |
| `auto_start` | `GROK_RC_AUTO_START` | `true` 则会话一开始就连，不必敲 `/rc` |
| `replay_buffer` | — | 重连时回放的帧数，默认 2048 |

环境变量优先于配置文件；空字符串当作没设置。

### 4. 在 grok 里打开遥控

按 newgrok 的方式启动 TUI（一般是仓库里的 `make run`），进入一个会话后：

```
/rc
```

也可以 `/rc on`、`/rc off`、`/rc status`。终端里应出现已连接的系统提示，**TUI 本身继续能用**——这是整件事情的前提。

然后打开 glance 的网页（默认 `http://127.0.0.1:7717`），用验证器里的 6 位码登录。会话列表里出现刚连上的 grok 即可遥控。

## 用的时候会怎样

- 浏览器发的 prompt 会出现在终端里，两边一起流式输出。
- 需要批准的工具调用、`ask_user_question`、退出 plan mode：终端和浏览器同时弹出。谁先答谁算数，另一边的卡片自己收起来。
- 浏览器里的 **Stop** 是真正的 `session/cancel`，会话日志会记成 `client:glance`，不是模拟按 Esc。
- glance 挂了、网络断了、进程被杀，终端不受影响。`/rc` 桥在自己的任务里退避重连；遥控不能把本地会话一起带走。

同一把 API key 重连会顶掉上一条连接。两台 grok 共用一把 key 会互相抢槽位，每台各发一把。

## 命令

```
glance serve [flags]            跑服务器
glance apikey add <name>        给一台 grok 签发 key
glance apikey list
glance apikey rm <id-or-name>
glance bootstrap                再打一次 setup token（仅未登记时）
glance version
```

`serve` 的 flag：

| Flag | 默认 | 说明 |
|---|---|---|
| `--addr` | `127.0.0.1:7717` | 监听地址。默认只绑回环，对外暴露必须是有意识的 |
| `--dir` | `~/.grok/glance` | 状态目录 |
| `--insecure-cookie` | 关 | 纯 HTTP localhost 用；非回环地址禁止 |

## 状态与恢复

持久化的只有无法重算的东西，写在 `~/.grok/glance/state.json`（`0600`）：TOTP 密钥、cookie 签名密钥、API key 的哈希。对话历史是每条 agent 内存里的环形缓冲（4096 帧），刷新页面会回放，**重启服务器不会**。

验证器丢了：删掉 `state.json`，重新登记。没有别的找回方式。这会同时作废所有 API key 和已签发的 cookie。`glance bootstrap` 在已经登记过后会拒绝再发 token。

`~/.grok/glance/` 里还有 `secret.key` 和 `hook.secret`，那是别的东西用的，不要动。

登录失败全局限流（5 分钟 8 次）。用过的 TOTP 步进会烧掉，同一个 30 秒窗口里登两次，第二次会失败——这是防重放，不是故障。

## 部署注意

glance 是带壳 agent 的遥控台。能通过鉴权的人可以批准任意工具调用。

- 默认只听 `127.0.0.1`。要暴露出去，放在终止 TLS 的反代后面。
- 自己不终止 TLS。公网上开 `--insecure-cookie` 等于把 session cookie 明文送出去。
- 没有多用户、没有角色、没有审计日志、没有按 key 分权限。
- Cookie 名是 `__Host-glance`：浏览器强制 `Secure` + `Path=/` + 不许设 `Domain`。改其中任何一项，浏览器会静默丢掉 cookie，看起来像登录坏了。

## 开发

```sh
make build        # 前端，再编二进制 → bin/glance
make server       # 只编 Go，沿用上次的前端
make web          # 只编前端
make check        # go vet + go test + tsc --noEmit
make dev          # Go + Vite 热更新 → http://localhost:5173
```

`//go:embed` 在编译期解析，所以 **`go build` 打进去的永远是上次 `make web` 的产物**。改了 UI 却没出现在 `bin/glance` 里，就是这个原因。`vite build` 会清空 `web/dist/`，连 `.gitkeep` 一起删；`make web` / `make clean` 会补回来，裸跑 `npm run build` 不会。干净 clone 缺这个文件时，`go build` 会报一个难看的 embed 错误。

前端开发用 `make dev`：Vite 把 `/api` 代理到 Go。`__Host-glance` 是 `SameSite=Strict`，跨源发不出去。拿 Vite UI 直接打 Go 的 7717 端口，会表现为莫名其妙的鉴权失败。

不编 Rust、只调 UI 时，用 `cmd/fakeagent`：它按真桥一样拨 agent socket，演一段带真实 `session/request_permission` 的回合。

```sh
bin/glance serve --insecure-cookie &
make fakeagent KEY=glance_sk_…
```

这证明不了真桥说的是同一种方言。动过桥、交互路径或线格式之后，按 [ARCHITECTURE.md](ARCHITECTURE.md) 的设计说明和仓库里的端到端清单，对着一台真的 newgrok 跑一遍。

设计上为什么是这个形状——grok 当拨出方、角色对终端是反的、浏览器不直接讲 ACP、交互两边抢答——见 [ARCHITECTURE.md](ARCHITECTURE.md)。
