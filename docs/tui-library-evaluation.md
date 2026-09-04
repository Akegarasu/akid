# TUI 技术选型

## 结论

v0.2 已采用 Charmbracelet 的 v2 组件栈：

```text
charm.land/bubbletea/v2  v2.0.9
charm.land/bubbles/v2    v2.2.1
charm.land/lipgloss/v2   v2.0.6
```

这些是选型时通过 `go list -m -versions` 确认的稳定版本，现已加入项目依赖。

这三个版本的 `go.mod` 都要求 Go 1.25，因此项目最低 Go 版本已从 1.23 提升到 1.25。项目仍处于早期阶段，直接从 v2 开始比先使用 v1、随后迁移更合适。

## 选择理由

### Bubble Tea v2

- Elm Architecture 的 `Model / Update / View` 与 daemon 的事件订阅模型天然对应。
- protocol event、log append、窗口变化和键盘输入都可以转成 `tea.Msg`，UI 状态仍由单一 update loop 修改。
- 支持全屏/inline、键盘、鼠标和高性能 cell renderer。
- v2 原生提供基于 OSC 52 的 `tea.SetClipboard`，正好覆盖设计文档中的 SSH 剪贴板需求。
- `tea.View` 可以声明 alternate screen、鼠标等终端行为，不需要在业务代码中手工维护 escape sequence。

### Bubbles v2

建议只使用适合的基础组件，而不是让组件拥有业务状态：

- `table`：进程列表的初始实现。
- `textinput`：搜索输入。
- `viewport`：详情页和普通文本区域。
- `help` / `key`：快捷键说明。
- `spinner`：初始连接和加载状态。

日志查看器仍应自行实现 `LogBuffer`、byte offset 分页、generation 校验、横向滚动和选择状态。通用 viewport 不应成为日志数据模型。

### Lip Gloss v2

用于边框、颜色、排版和状态栏。样式只留在 view 层，不能进入 protocol 或日志 buffer。

## 其他候选

| 方案 | 优点 | 不采用的主要原因 |
|---|---|---|
| `rivo/tview` | Table、TextView、Grid 等高层 widget 很完整，上手快 | imperative widget/event 模型；自定义日志分页、Vim 式选择和 daemon 事件整合时，更容易出现多处可变状态 |
| `gdamore/tcell/v3` | 底层控制强、Unicode 和键盘支持好 | 层级过低，需要自行实现完整组件、布局和 update loop；更适合作为框架底座 |
| `gocui` | API 小、可自定义 view | 组件生态和活跃度不如前两者，不适合把日志查看器作为核心功能的新项目 |

`tview` 是可行的第二选择；如果产品目标只是表格加文本框，它会更省代码。但 akid 的重点是带分页、follow、搜索、选择与复制的日志查看器，因此 Bubble Tea 的显式状态机更匹配。

## 建议目录

```text
internal/tui/
├── app.go                 # 顶层 Model/Update/View
├── commands.go            # protocol 调用转换为 tea.Cmd
├── messages.go            # process/log/protocol 消息
├── keymap.go
├── styles.go
├── process_list.go
├── process_detail.go
├── log_viewer.go
├── log_buffer.go          # 不依赖 Bubble Tea，纯状态与分页逻辑
└── clipboard.go           # tea.SetClipboard + 外部命令 fallback
```

边界保持为：

```text
TUI -> protocol.Client -> daemon
```

`internal/tui` 不得 import `internal/manager`、`internal/storage` 或直接读取日志文件。

## 实施状态

已完成：

1. `akid ui` Cobra 子命令与 Bubble Tea v2 全屏应用。
2. `process.list`、`event.subscribe`、进程列表与详情页。
3. start/stop/restart 交互命令。
4. 与 UI 框架无关、带单元测试的有界 `LogBuffer`。
5. `log.read` 历史分页、`log.subscribe`、generation rotation 校验与 follow。
6. 已加载窗口搜索、行/字符选择、外部 clipboard 和 OSC 52 fallback。
7. Linux `/proc` CPU/RSS 采样与展示。

鼠标选择仍保留为后续可选增强；键盘选择是当前的一等交互。

## 官方资料

- Bubble Tea: <https://github.com/charmbracelet/bubbletea>
- Bubbles: <https://github.com/charmbracelet/bubbles>
- Lip Gloss: <https://github.com/charmbracelet/lipgloss>
- tview: <https://github.com/rivo/tview>
- tcell: <https://github.com/gdamore/tcell>
