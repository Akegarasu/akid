# akid

`akid` 是一个面向 Linux 单机、单用户环境的轻量级进程管理器。功能范围与开发记录见 [`docs/development-status.md`](./docs/development-status.md)。

## 当前实现

当前实现覆盖 v0.1 Supervisor Core、v0.2 TUI、v0.3 声明式配置及 v0.4 协议完善：

- Unix Domain Socket + NDJSON method protocol
- Cobra 子命令/参数解析、daemon 单实例锁与 CLI 自动拉起
- `create/start/stop/restart/delete/list/status`
- `always`、`on-failure`、`never` 重启策略及指数 backoff
- Linux process group、`SIGTERM`/超时 `SIGKILL`、child subreaper
- `PID + /proc start_time` 身份校验和 daemon 崩溃后的进程 adopt
- 原子 `state.json` 持久化、上一代备份与损坏恢复
- stdout/stderr 直接写文件
- 分页日志读取、日志订阅和 `logs -f`
- size-based copytruncate rotation
- 独立的状态事件与日志订阅连接及有界队列
- Bubble Tea v2 TUI：进程列表、详情、启停/重启和实时状态事件
- 日志浏览器：2 MB / 10000 行有界窗口、历史分页、follow、搜索和横向滚动
- Vim 风格字符/整行选择、外部 clipboard 与 OSC 52 fallback
- Linux `/proc` CPU / RSS 指标采样
- TOML 配置校验及幂等 `apply`，配置更新保留进程 ID 并重启
- systemd user service 的 startup install/uninstall 与 linger 状态提示
- 协议版本 2、能力查询、订阅背压与连接关闭处理

本地原始设计与 TUI 选型资料保存在 `.agents/docs/`；该目录不纳入 Git。可选 WebUI 属于后续版本。

## 构建

要求 Go 1.25 或更高版本（Bubble Tea v2 的最低要求）。正式运行目标是 Linux。

```bash
go build -o akid ./cmd/akid
```

## 使用

```bash
# 创建并启动
./akid start ./server --name api --restart always

# 以 “-” 开头的子进程参数必须放在 -- 后面
./akid start python worker.py --name worker -- --port 8080

./akid list
./akid status api
./akid restart api
./akid stop api
./akid delete api
./akid delete api --purge

./akid logs api
./akid logs api --stderr -n 200
./akid logs api -f

# 打开全屏 TUI；鼠标捕获默认关闭，按需启用
./akid ui
./akid ui --mouse

./akid daemon status
./akid daemon stop
```

### 声明式配置

配置示例见 [`examples/akid.toml`](./examples/akid.toml)。

```bash
./akid apply --check akid.toml  # 只校验，不连接或启动 daemon
./akid apply akid.toml
./akid apply                  # 默认读取当前目录的 akid.toml
```

每个 `[[process]]` 使用唯一名称；支持 `command`、`args`、`cwd`、`restart`、
`stop_timeout` 和 `[process.env]`。未知字段、重复名称和非法参数会在应用前拒绝。
`cwd` 相对配置文件目录解析，省略时使用该目录；包含 `/` 的相对 command 按子进程
`cwd` 执行，裸命令名从 daemon 的 PATH 查找。不会展开 shell 变量或拆分参数。

不存在的进程会创建并启动；配置相同则不操作（手动停止的进程也保持停止）；
配置改变则更新并重启。配置中没有列出的进程保留，当前不提供 `--prune`。
所有配置先校验并保存，再执行进程操作；启动失败逐项报告，其他条目可以成功，
不回滚已执行的进程操作。更新正在运行的进程时，CLI 等待重启结果；协议响应表示
操作已接受，后续状态通过事件或查询获取。daemon 崩溃后会继续已保存的待重启操作。

### 开机启动

先将二进制放在长期保留的位置，再执行：

```bash
./akid startup install
./akid startup uninstall
```

安装会写入 `$XDG_CONFIG_HOME/systemd/user/akid.service`（默认
`~/.config/systemd/user/akid.service`），执行 user daemon-reload 和 enable。
服务从下次用户会话或开机开始运行；当前 daemon 不会被接管或重启。
若需要立即切换，先运行 `akid daemon stop` 并等待其退出，再运行
`systemctl --user start akid.service`。卸载会 disable 并移除生成的 unit，保留当前运行状态和数据。
已有非 akid 生成的 unit 会被保护，不会覆盖。

unit 使用 `Restart=on-failure`，保存安装时的状态和 socket 目录配置。
无人登录时运行需要开启 linger；安装会检测并提示，可按提示执行
`loginctl enable-linger "$USER"`。helper 不自动修改 linger。

数据默认位于 `$XDG_STATE_HOME/akid`（fallback 为 `~/.local/state/akid`），socket 优先位于 `$XDG_RUNTIME_DIR/akid.sock`。

### TUI 快捷键

- 进程列表：`↑↓`/`jk` 选择，`Enter` 日志，`i` 详情，`a/r/s` 启动/重启/停止，`/` 实时过滤。
- 进程详情：`↑↓`/`jk`、`PgUp/PgDn` 和 `g/G` 浏览较长配置与环境变量。
- 日志：`↑↓`/`PgUp`/`PgDn` 浏览，`g/G` 顶部/底部，`f` follow，`1/2` 切换 stdout/stderr。
- 日志搜索与复制：`/` 搜索，`n/N` 匹配跳转，`v/V` 字符/整行选择，`y` 复制，`w` 写入临时文件。
- `Esc` 返回或取消选择，`q` 退出/返回。
- `--mouse` 启用点击选择和滚轮浏览；默认关闭，以免妨碍终端原生文本选择。

## 测试

```bash
go test ./...
go test -race ./...
go vet ./...
go build -o /tmp/akid-test ./cmd/akid
python3 scripts/test-linux-integration.py /tmp/akid-test
```

Linux 端到端脚本使用临时 XDG 目录和测试 daemon，startup 部分使用 systemd 命令替身。
Linux 原生验证记录见 [`docs/linux-test-results.md`](./docs/linux-test-results.md)。

从非 Linux 主机检查 Linux 构建：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
```

## 对设计方案的一点修正

新 daemon 无法通过事后设置 subreaper 来 `waitpid` 已经被旧 daemon 遗留并 reparent 给 PID 1 的进程。因此实现中：

- 当前 daemon 直接创建的进程由统一 `wait4` reaper 回收；
- subreaper 收割运行期间 reparent 到当前 daemon 的孙进程；
- 从旧 daemon adopt 的进程通过 `/proc/<pid>/stat` 中的 `start_time` 定时校验存活状态。

这样避免了把“adopt 后可以直接 waitpid”作为不成立的前提。

另外，当 `restart=never`，或 `restart=on-failure` 的进程正常退出时，实现会把 `desired` 一并置为 `stopped`。否则现有持久化模型只留下 `desired=running + hint=nil`，daemon 下次启动会错误地再次启动该进程。

日志订阅 cursor 属于每个客户端连接，而不是进程的单一全局属性；当前由客户端使用 `offset + generation` 续传。服务重建或轮转造成无法连续读取时，订阅发送 `log.gap`，客户端从当前代次重新读取。

状态订阅建立后先发送 `process.snapshot`，后续事件带 daemon epoch 与进程 revision；删除完成发送 `process.deleted`，客户端据此移除旧行并忽略过期响应。
