# Linux Test Results

最新一轮 v0.3/v0.4 开发验证结果见本文末尾「后续开发验证」。以下为此前 Linux
交接验证的历史记录；其中“无代码修改”等描述仅适用于首次交接验证。

Date: 2026-09-05, Asia/Shanghai (UTC+08:00).
Final native checks completed at 16:40:37.

All requested Linux native checks passed. No code fixes were needed. An initial
sandbox socket restriction was resolved by rerunning the checks outside the
sandbox as the same non-root user; no required check remains blocked or skipped.

## Revision and workspace

- Repository: `/home/akiba/akid`.
- Commit: `34bed0478db550ca22d27a3d7b06fec24bd7c35a` (`34bed04 update`).
- Initial `git status --short`: `?? README.md`; `git diff --stat` was empty.
- The Linux executor tests, TUI text-width helper, and latest daemon subscription
  assertions were already present in tracked files at this commit.
- `README.md` was present as an untracked file. As confirmed by the user, the
  design and TUI evaluation documents are in `.agents/docs/akid-design.md` and
  `.agents/docs/tui-library-evaluation.md`, rather than the handoff's old paths.
  `.agents` is ignored by Git; both documents were inspected in place.
- Protocol version remains `2` in `internal/protocol/protocol.go`.
- This run adds only `docs/linux-test-results.md`. Existing source files,
  `README.md`, and `.agents/docs/` documents were preserved. Handoff file hashes
  were captured and verified unchanged after testing.
- No commit, push, deployment, cleanup, or rollback was performed.

## Environment

| Item | Value |
| --- | --- |
| Distribution | Ubuntu 22.04.1 LTS (Jammy Jellyfish) |
| Kernel | Linux 5.15.0-105-generic, #115-Ubuntu SMP Mon Apr 15 09:52:04 UTC 2024 |
| Host architecture | x86_64 |
| Go | go1.27.1 linux/amd64; module declares go 1.25.0 |
| GOOS / GOARCH | linux / amd64 |
| CGO_ENABLED | 1; explicitly set to 1 for the race run |
| C compiler | /usr/bin/cc, Ubuntu GCC 11.4.0-1ubuntu1~22.04 |
| User | akiba, UID 1000; no sudo |

Artifacts: `/tmp/akid-linux-test.jqJ4se`.
`GOCACHE` was set to the artifact directory's `go-build-cache` subdirectory so
compiler output remained outside the repository in a writable location. Final
checks ran sequentially in one Bash session with `pipefail`, using
`run-native-checks.sh` in the artifact directory. `results.tsv` records exit
statuses and completion timestamps.

## Commands and results

Here `$akid_test_artifacts` denotes `/tmp/akid-linux-test.jqJ4se`.

| Command | Result | Log |
| --- | --- | --- |
| `go test -v -count=1 -timeout=3m ./internal/executor` (initial sandbox run) | PASS; all 5 tests, 3.120s | `executor.log` |
| `go test -count=1 -timeout=5m ./...` (initial sandbox run) | ENVIRONMENT FAILURE; daemon could not create a loopback TCP socket; all other tested packages passed | `tests.log` |
| `go test -v -count=1 -timeout=3m ./internal/executor` (outside sandbox) | PASS; all 5 tests, 3.140s | `executor-native.log` |
| `go test -count=1 -timeout=5m ./...` (outside sandbox) | PASS; all 9 packages with tests | `tests-native.log` |
| `CGO_ENABLED=1 go test -race -count=1 -timeout=10m ./...` | PASS; all 9 packages with tests, no race reports | `race.log` |
| `go vet ./...` | PASS; no diagnostics | `vet.log` |
| `go build -o "$akid_test_artifacts/akid" ./cmd/akid` | PASS; native Linux executable | `build.log` |
| `"$akid_test_artifacts/akid" --help` | PASS; usage and available commands printed | `help.log` |
| `gofmt -l internal cmd` | PASS; no output, no files reformatted | `gofmt.log` |
| `git diff --check` | PASS; no output | `diff-check.log` |
| `git status --short` | PASS; existing untracked README only before adding this report | `status.log` |

`internal/model` and `internal/paths` report `[no test files]`; these are not
skipped tests. No test command timed out.

The initial failure was `TestServerDispatchOverSocket`, at
`internal/daemon/server_test.go:52`:

```text
listen tcp 127.0.0.1:0: socket: operation not permitted
```

The execution sandbox prohibits this socket operation. Approval to run the
concrete test script outside the sandbox was granted, and the unchanged daemon
test passed in both the native full run and race run. The executor was also
rerun outside the sandbox to validate against the VM's normal process namespace.

## Linux executor coverage and process cleanup

These tests executed natively, and also ran in the full and race suites:

| Test | Behavior asserted |
| --- | --- |
| `TestQuickExitPreservesStatus` | 20 quick exits retain their actual codes (0, 1, 2) and a nonzero process start-time identity. |
| `TestLeaderExitCleansTermIgnoringDescendant` | A leader exiting with code 7 triggers cleanup of a TERM-ignoring descendant; after Done, that descendant is no longer live and code 7 is retained. |
| `TestStopStillSignalsGroupAfterLeaderExit` | Group stop handles a TERM-ignoring descendant, rejects an incorrect start-time token with ErrProcessGone, and reports leader signal exit code 143. |
| `TestTrackedMembersRejectsReusedGroup` | Synthetic /proc entries with reused identities are rejected; a known remaining member still anchors ownership after the leader disappears. |
| `TestAdoptedLeaderExitCleansKnownMembers` | A leader created by a separate fixture process can be adopted and stopped; Done reports an unknown exit code and its known descendant is no longer live. |

Process snapshots include PID, PPID, PGID, SID, state, elapsed time, and arguments.
Comparison with the baseline after the standalone executor, full suite, race
suite, and final checks found no surviving test-created processes and no
zombies. The only newly present PID at each snapshot was the snapshot's own
`ps` command. No process cleanup was necessary. A pre-existing akid daemon
(PID 5692) was present in the baseline and was left untouched.

Evidence: `processes-before.log`, `processes-after-executor.log`,
`processes-native-before.log`, `processes-native-after-executor.log`,
`processes-native-after-tests.log`, `processes-native-after-race.log`,
`processes-native-final.log`, and `process-residue-review.log`.

## Cross-platform regression coverage

Existing tests passed on Linux for manager interrupted-stop recovery and stale
hints, deletion/name reservation, ordered snapshots and deletion events;
logging long-line pagination, unterminated lines, generation changes, gaps,
ownership checks and subscriber closure; and TUI stale responses, deletion of
the viewed process, bounded long-line buffers, wide-character selection and
horizontal scrolling.

The daemon socket test passed its initial `process.snapshot`, subsequent
`process.deleted`, protocol version 2, matching epoch, and increasing revision
assertions. The race suite included executor, manager, logging, daemon and TUI.

## Changes and remaining scope

No implementation or test changes were necessary. The permission failure was
environmental and did not require a code workaround or longer timeouts.

There are no remaining environment-blocked checks from the handoff. This run
does not establish behavior on other kernels or architectures, and did not
repeat the previous Windows validation. PID reuse rejection was tested with
synthetic identities, not by forcing actual kernel PID reuse. Interactive TUI
terminal/clipboard behavior and deployment were not exercised; TUI regression
tests and CLI help were executed as requested.

Additional evidence: `environment.log`, `environment-native.log`,
`handoff-files.sha256`, `run-native-checks.sh`, `results.tsv`, and the built
`akid` executable, all in the artifact directory. Final report/working-tree
checks are recorded in `diff-check-final.log`, `status-final.log`, and
`handoff-integrity-final.log` there.

## 后续开发验证：v0.3 / v0.4

时间：2026-09-05 17:12（Asia/Shanghai）；基于同一提交
`34bed0478db550ca22d27a3d7b06fec24bd7c35a` 的未提交工作区。
环境仍为 Ubuntu 22.04.1、Linux 5.15.0-105-generic、amd64、Go 1.27.1，
普通用户 akiba（UID 1000），race 使用 CGO_ENABLED=1。

本轮完成 TOML 配置与 apply、systemd startup helper、协议能力查询与连接生命周期
完善，并修复审查中发现的日志 owner/cursor 和待重启恢复问题。功能细节及语义边界
见 [development-status.md](./development-status.md)。

测试日志、检查脚本与构建产物位于 `/tmp/akid-development.wHdPUQ`，
`GOCACHE=/tmp/akid-development.wHdPUQ/go-build`。最终检查按 `verify.sh`
顺序执行，均使用 `-count=1`；日志状态和完成时间保存在 `results.tsv`。

### 最终命令结果

| 检查 | 结果 | 日志 |
| --- | --- | --- |
| `go test -v -count=1 -timeout=5m ./...` | 通过：11 个含测试的包，81 个顶层测试（另含子测试），无跳过/超时 | `tests.log` |
| `CGO_ENABLED=1 go test -race -count=1 -timeout=10m ./...` | 通过：无竞态报告 | `race.log` |
| `go vet ./...` | 通过：无诊断 | `vet.log` |
| `go build -o "$artifacts/akid" ./cmd/akid` | 通过：Linux 原生构建 | `build.log` |
| `"$artifacts/akid" --help` | 通过：包含 apply/startup | `help.log` |
| `"$artifacts/akid" apply --check examples/akid.toml` | 通过：2 个合法条目，不连接 daemon | `example-check.log` |
| `python3 scripts/test-linux-integration.py "$artifacts/akid"` | 通过：隔离 XDG、真实 daemon/进程、无存活测试组成员 | `integration.log` |
| `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o "$artifacts/akid-windows.exe" ./cmd/akid` | 通过：仅交叉构建，未在 Windows 执行 | `windows-build.log` |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$artifacts/akid-static" ./cmd/akid` | 通过：无 CGO 构建 | `linux-static-build.log` |
| `gofmt -l internal cmd` | 通过：无输出 | `gofmt.log` |
| `git diff --check` | 通过：无输出 | `diff-check.log` |
| `systemd-analyze verify "$artifacts/unit-check/config/systemd/user/akid.service"` | 通过：沙箱外只读校验，无诊断 | `unit-verify-native.log` |

表中 `$artifacts` 表示 `/tmp/akid-development.wHdPUQ`。
`internal/model` 和 `internal/paths` 无独立测试文件，相关模型验证被 config、manager
和 storage 测试使用。

### 新功能与修复的回归证据

- Config：配置文件相对 cwd、默认值、参数与环境变量保留、未知字段/重复名称/
  非法超时/错误类型拒绝、8 MiB 文件上限。CLI `--check` 不创建状态目录。
- Apply：create/no-op/update、默认超时等价、更新保留 ID、未列进程保留、停止后同配置
  no-op、非法批次无副作用、首次持久化失败保留旧进程、部分启动失败逐项返回、
  并发 apply 只启动一次、恢复已持久化的待重启配置。
- Startup：重复安装/卸载、非托管 unit 保护、systemd 展开符转义、失败返回与重试、
  linger 提示。单元测试使用 mock runner，端到端 CLI 使用临时 PATH 中的替身命令。
- Protocol/daemon：能力响应、空 apply、缺失参数、旧协议拒绝、Server.Close 关闭控制和
  订阅连接、慢订阅写超时；现有初始快照、删除事件、epoch/revision 测试持续通过。
- Logging：旧 owner 不能读取或订阅同名新进程日志；越界 cursor 发 gap 后从开头续读。
- 原有 5 个 Linux executor 原生用例在全量及 race 中执行，分别包括真实退出码、
  TERM-ignoring 成员清理、组长退出、身份重用拒绝和接管后的清理。
- 端到端脚本实际测试 daemon 被 SIGKILL 后的 adopt、stop、配置改变后的重启、日志
  输出、应用失败后的恢复、delete --purge 及正常 shutdown。

实现修改位于 `internal/config`、`internal/manager/apply.go`、
`internal/manager/manager.go`、`internal/model/model.go`、`internal/startup`、
`internal/protocol`、`internal/daemon`、`internal/logging/service.go` 和 CLI 新命令；
相应测试、示例与文档已加入。配置更新的待重启意图使用可选 `restart_pending`
持久化字段；协议保持版本 2。daemon 现在先绑定 socket 再恢复进程，避免绑定失败后
留下已经恢复却不可访问的进程。

### 工作区、残留与未验证事项

最终检查前后进程快照只新增了快照命令自身的 PID，没有新增存活测试进程或 zombie，
详见 `processes-before.log`、`processes-after.log` 和 `process-review.log`。
原有 daemon 保留，未连接或修改其状态。本轮没有提交、推送、部署或实际安装用户服务。

工作区包含新增实现/测试/示例/文档与既有文件修改；README 原本未跟踪，本轮仅同步
新功能说明。`.agents/docs/akid-design.md` 和 TUI 评估文件的 SHA-256 与首次交接记录
一致，原始文档保留。最终状态与源码指纹见 `status-final.log` 和 `workspace.sha256`。

受沙箱 socket 限制的检查已在沙箱外以普通用户完成。首次 unit 语法检查在沙箱内
虽返回 0，但伴随 socket 权限诊断（`unit-verify.log`），因此最终结论使用沙箱外无
诊断的 `unit-verify-native.log`。不存在仍受环境阻挡的必需代码检查。

尚未验证：真实 systemd enable/启动、用户登出、开机及 linger 行为；Windows 原生
执行；其他内核/架构；真实交互终端的剪贴板。这些不计入“测试通过”的结论。
可选 WebUI 与 prune 未进入本轮开发范围。
