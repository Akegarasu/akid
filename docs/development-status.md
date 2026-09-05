# 开发完成范围

本轮在提交 `34bed0478db550ca22d27a3d7b06fec24bd7c35a` 的工作区上继续开发，
补齐设计文档中的 v0.3、v0.4。既有 v0.1 核心和 v0.2 TUI 保留并执行回归。
原始设计与 TUI 评估文件在本地 `.agents/docs/`，本轮未改写。

## 已实现

| 阶段 | 当前实现 |
| --- | --- |
| v0.1 | 进程生命周期、重启策略/backoff、持久化、Linux 进程组/subreaper/adopt、CLI、日志读取与轮转 |
| v0.2 | 进程列表与详情、分页日志、follow/search/selection/copy、宽字符处理、CPU/RSS |
| v0.3 | TOML 解析与严格校验、配置文件相对 cwd、幂等 apply、systemd user startup helper |
| v0.4 | 协议版本 2、能力查询、统一参数错误、状态快照/revision、日志 owner 校验与 gap、订阅连接超时和关闭 |

## Apply 语义

- `akid apply [FILE]` 默认读取 `akid.toml`；`--check` 不连接 daemon。
- 所有条目先校验，重复名称、未知字段、非法环境变量和超时会拒绝。
- 以 name 匹配：新增时 create + start，相同则 no-op，改变则 update + restart。
  手动停止后应用相同配置仍保持停止；需要启动时使用 `akid start NAME`。
- 更新保留 ID、日志归属和订阅身份；没有列出的进程保留，不提供 prune。
- manager 在单一事件循环中处理整批配置，先保存配置与运行意图，再执行进程操作。
  配置准备或首次保存失败会恢复内存配置、释放本次新注册名称，不停止旧进程。
- 运行期失败逐项返回；配置保存不等于所有程序均已成功运行，不提供进程副作用事务回滚。
  原有 restart policy 继续生效；相同配置不会反复重试已经终止的 `restart=never` 进程。
- `restart_pending` 随 state.json 保存，恢复时先完成旧进程组停止，再启动新配置。
  该字段是可选的兼容扩展；state 文件版本仍为 1，**daemon 协议版本仍为 2**。
- `config.apply` 返回每项 action、当前 ProcessInfo 和可选 error。正在运行的进程
  更新可能先返回 stopping；CLI 会等待重启结束，其他客户端通过状态事件跟踪。

## 协议扩展与订阅

`daemon.capabilities` 无参数，返回：

```json
{
  "protocol": 2,
  "methods": ["daemon.ping", "daemon.capabilities", "config.apply"],
  "features": ["process.snapshot", "process.deleted", "process.revision", "log.gap", "log.partial", "subscription.lagged"],
  "max_message_size": 16777216
}
```

上面 methods 只展示部分；实际响应列出全部支持的方法。客户端应查询能力后再使用
扩展方法，不能只凭协议版本推断扩展已存在。`apply` 已接入该检查。

`config.apply` 参数为 `{"processes": [ProcessConfig, ...]}`，ID 必须省略。
空数组是合法 no-op；缺失/null 数组拒绝。TOML 由 CLI 转换成 ProcessConfig 后发送，
daemon 不访问客户端配置路径。参数校验统一返回 `INVALID_CONFIG`，重复名称返回
`PROCESS_NAME_CONFLICT`；启动失败以逐项 `SPAWN_FAILED` 返回。

控制连接最多同时处理 32 个请求；每次响应/订阅写入限时 5 秒，接收停止后连接释放。
订阅队列维持 1024 的上限；溢出发 `event.lagged`，关闭连接并由客户端续传。
Server.Close 同时取消请求并关闭已接受的连接。事件客户端校验每帧协议版本。

日志读取及订阅在获取 fileState 时验证 process owner，避免旧请求跟随同名新进程。
代次改变或订阅 cursor 超过当前 EOF 会发送 `log.gap` 后从当前文件开头续读。
日志订阅要求非负 offset 和有效 stream；单次 log.read 仍支持负 offset 从末尾读取。

## Startup helper

安装只生成 user unit 并执行 `systemctl --user daemon-reload/enable`，不使用 `--now`。
这样当前由 CLI 自动启动的 daemon 可以继续工作，不会触发两个实例争抢锁。
卸载执行 disable、删除本工具生成的 unit 并 reload，不停止 daemon，也不删除数据。
非本工具生成的 unit 和符号链接会拒绝覆盖或删除。

unit 使用当前二进制的绝对路径，转义 systemd 的 `%`、`$`、引号和反斜杠，
固定安装时的 XDG 状态/socket 设置，采用 `Restart=on-failure`、`KillMode=mixed`、
`TimeoutStopSec=75s` 和 `UMask=0077`。linger 只检测和提示，不自动修改。
systemctl 操作失败会返回错误；生成文件保留以便重试。二进制移动后应重新安装 unit。

## 验证与边界

新增测试覆盖 TOML 校验、幂等/并发 apply、状态保存失败、逐项运行失败、待重启恢复、
unit 保护和 systemd 参数转义、能力查询、协议不匹配、连接关闭与慢订阅、日志 owner
和越界 cursor。可重复执行的 `scripts/test-linux-integration.py` 使用真实 Linux
daemon 和进程测试 CLI/IPC/日志/崩溃接管，systemd 安装流程使用替身命令。

详细命令和结果见 `linux-test-results.md`。真实 systemd 开机/登出、跨主机运行和
交互终端中的剪贴板行为仍需要对应环境验证，不能由模拟命令或单元测试代替。

可选 WebUI（v0.5）、prune、远程访问、认证、多用户、资源限制和部署自动化仍在本轮
范围之外。copytruncate 的写入窗口、stdout/stderr 无精确合并顺序、逃离进程组的
独立 session 等既定边界仍然存在。

## CLI 使用补充

`start` 的第一个参数支持轻量 shell 风格的命令字符串。例如
`akid start "uv run bot.py" --name chino-bot` 会保存 command=`uv`、args=`["run", "bot.py"]`。
解析只处理空白、单/双引号和反斜杠，不执行 shell 展开、管道或重定向。
对不含 `/` 的命令，CLI 先用自己的 PATH 解析成绝对路径再交给 daemon；这样 daemon
由旧环境或 systemd 启动时仍可找到 `$HOME/.local/bin/uv`。找不到的命令保留原名，
最终返回正常的 `SPAWN_FAILED`。

`list` 的别名包括 `ls` 和 `ps`，输出按名称排序并带 1 起始序号。CLI 接受名称、ID
或序号；先尝试精确名称/ID，数字没有同名进程时按当前列表序号解析。TUI 详情页
同时展示 ID、args、cwd、时间、退出码、日志 generation 和环境变量。
