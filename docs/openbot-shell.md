# Openbot 衍生外壳（可选入口）

在页面加载时使用 `?shell=openbot` 选择；其他值或未指定时保留默认 IM。不会监听 URL 变化或提供运行中换壳按钮。首次加载可选 JS/CSS 后才挂载 App；分块载入失败会显示中文错误及默认入口链接，不自动创建另一个 App。

## 来源与许可

固定来源：[nightly-labs/openbot](https://github.com/nightly-labs/openbot/tree/ac99196e66fe8f9d426433aa5e79d6e999d1c818)，提交 `ac99196e66fe8f9d426433aa5e79d6e999d1c818`。这是实际源码提取后进行的薄 React JSX 适配，不是只模仿配色。

**PolyForm Noncommercial 1.0.0，不是 MIT。** 已获用户个人研究及非商业复用授权；不据此宣称商业用途获准。完整、未修改的 `LICENSE`、`NOTICE` 和固定源文件 SHA-256 清单随 `web/public/licenses/openbot/` 分发；轨道上始终提供“开源许可”链接（`/licenses/openbot/index.html`，兼容后端只服务普通文件的规则），在新标签页打开以免卸载未完成操作。两条 Required Notice 均完整保留。校验值：

- `LICENSE`：`ffe034ecf735f296ca53cbd856c944849f0c80c7e1490ce162038a99ad89c728`
- `NOTICE`：`3995ad096839d696f4ff70227ac56c89b3c25e6b549bb87cf9d98dce901b75f7`

NOTICE 是保守保留的完整上游包声明，不是本切片的依赖清单。未复制品牌标志、头像、字体、第三方图标、Blobatar、图表、可选 provider 运行时、Docker seccomp、光标动效或分析代码。`SOURCE.json` 描述只读参考源文件，不代表全部进入分发包。

### 提取映射

以下路径与行号均相对上述固定提交；可通过固定仓库 URL 的 `blob/<commit>/<path>#L<start>-L<end>` 核对。完整源文件哈希见随包 `SOURCE.json`。

| 上游路径与范围 | 本地落点 | 保留结构与改动 |
| --- | --- | --- |
| [`src/renderer/src/WorkspaceFrame.tsx:11–39,62–103`](https://github.com/nightly-labs/openbot/blob/ac99196e66fe8f9d426433aa5e79d6e999d1c818/src/renderer/src/WorkspaceFrame.tsx#L11-L103) | `web/src/openbot-shell/WorkspaceFrame.tsx` | 保留 `left → app-frame-panes → content → children` 的真实绘制次序和调用方提供面板的槽位契约；增加跳转链接槽位；Solid JSX/ParentProps 换为 ReactNode。去掉平台、布局上下文、零宽 resizer、兼容性/连接屏、新页面覆盖层及其 inert 逻辑；App 的 hidden 状态继续决定可访问性。 |
| [`packages/ui/src/features/sidebar/SidebarFrame.tsx:1–16`](https://github.com/nightly-labs/openbot/blob/ac99196e66fe8f9d426433aa5e79d6e999d1c818/packages/ui/src/features/sidebar/SidebarFrame.tsx#L1-L16) | `SidebarFrame.tsx` | 保留 aside、sidebar/panel-edge 和子槽位；本地 `im-groups` 标识、中文标题、可见性与 focus ref 由 App 传入，无 agent 身份转换。 |
| [`packages/ui/src/features/window/PageRail.tsx:24–43,381–434`](https://github.com/nightly-labs/openbot/blob/ac99196e66fe8f9d426433aa5e79d6e999d1c818/packages/ui/src/features/window/PageRail.tsx#L24-L434) | `PageRail.tsx` | 提取真实 RailButton 的 label/current/callback 属性和 nav/group 结构；原生 button 替换 UI Button，React props 替换 Solid 控制流；可见中文文字取代 logo/图标/tooltip。增加 aria-controls/expanded 和 App 的触发器 refs；底部分组为来源链接。排除账号、设置、用量、固定项、市场、调度及其运行时。 |
| [`packages/ui/src/features/window/page-rail.css:1–14,47–92`](https://github.com/nightly-labs/openbot/blob/ac99196e66fe8f9d426433aa5e79d6e999d1c818/packages/ui/src/features/window/page-rail.css#L1-L92) | `openbot-shell.css` 的轨道段 | 保留 flex 轨道、分组、按钮及 hover/current/focus 规则顺序。去 Electron 拖拽、logo/icon 规则；映射现有 tokens，44px 触摸目标，短屏可滚动。 |
| [`src/renderer/src/styles/app-shell.css:24–46,848–854,867–887,3347–3350`](https://github.com/nightly-labs/openbot/blob/ac99196e66fe8f9d426433aa5e79d6e999d1c818/src/renderer/src/styles/app-shell.css#L24-L46) | `openbot-shell.css` 的 frame/sidebar 段 | 连续片段保留各自声明与结构先、表面后顺序；去零宽拖拽列，采用 rail/list/panes 三列、100svh，sidebar-topbar 映射现有 im-brand。本地 palette 映射到现有 tokens；不用 85KB 全局样式。 |
| [`src/renderer/src/features/web-client/web-client.css:61–97,112–130,274–278`](https://github.com/nightly-labs/openbot/blob/ac99196e66fe8f9d426433aa5e79d6e999d1c818/src/renderer/src/features/web-client/web-client.css#L61-L130) | `openbot-shell.css` 的 mobile/reduced-motion 段 | 适配单手机面板、列表时隐藏 panes 的规则/思路，使用原有 56rem 断点和 App pane；轨道一直可达。不引入底部导航、额外移动状态或认证界面。 |

所有新增样式选择器限定于 `.openbot-shell`。剩余样式段仅连接已有 im-* 槽位与有界滚动区；不改消息展现、滚动 hook 或输入组件。

## 状态与动作边界

- `App.tsx` 仍是 selected group、pane/context、lazy visits、read activity、focus recovery 的唯一控制器。新增 `AppShellProps` 只传 typed React slots、真实动作及 refs；可选模块不请求 API，不持有第二套状态，不查询或点击 DOM 代替动作。
- 默认布局保留原有结构与 header actions；可选布局将同一动作接到 PageRail，复用同一 Groups、Discussion/composer、Members、Tasks、HistorySearch。手机从列表打开详情时，仍由 App 设置 chat pane。轨道“聊天”关闭详情并恢复现有聊天；“群组列表”关闭详情并恢复当前群组按钮焦点。
- Shell 组件类型与 slot 位置在 pane/group 切换时稳定；不使用以 pane/group 为 key 的 shell，不重新挂载 DiscussionScope 或 mutation。关闭详情恢复到轨道的实际触发器。隐藏区域保留 DOM，但从键盘与可访问树移除；读取仍由原组件的 active 参数暂停，未完成写操作不会因布局切换 abort 或自动重试。
- 网络契约、轮询周期、幂等身份和未知结果分类均未改动。原始输入与 key 仅经用户显式同次重试再次发送；界面不把持久化说成 CLI 接收，也不把提交说成验收。
- 不添加依赖、Solid runtime、品牌或第三方资产、假账号/设置/主机/队列操作。本切片不使 Openbot 成为默认入口。

## 验证与集成交接

行为测试在 `web/src/openbot-shell/OpenbotShell.test.tsx`：从真实 main 入口载入，检查默认/可选/加载时选择，分块错误，桌面/手机轨道导航下的分群草稿、接收者、composer DOM 和 pending/unknown 消息/任务操作及同次 key/input 重试，以及断点与焦点恢复。只替换 fetch/viewport，记录真实 createRoot 便于卸载；不 mock App 或其业务组件。

运行命令（保持默认 5s 超时和单 worker）：

```sh
cd web
npm test -- --maxWorkers=1 src/openbot-shell/OpenbotShell.test.tsx
npm test -- --maxWorkers=1 src/App.test.tsx src/Discussion.test.tsx src/Discussion.scroll.test.tsx src/message-polling.test.tsx src/HistorySearch.test.tsx src/Members.test.tsx src/Tasks.test.tsx src/RequestWork.test.tsx
npm run typecheck
npm run build
```

构建前后大小、RED/GREEN 日志、失败原因、命令退出码与最终文件清单在本机授权证据目录 `.local/pi/openbot-shell-01/`，不进入版本库。合并前仍须协调者集成 #87，保留其 RequestWork 变更并重跑适用测试；由独立 Pi terra max 复核边界、来源与许可。真实浏览器须验证桌面/390px 手机/短高屏、Tab 与关闭/返回焦点、草稿及 pending/unknown、历史与滚动锚点、默认 CSS 无变化、可选 chunk 按需加载、许可页面与文件可访问。使用独立 fixture 服务，不修改或重启 7310，也不发起付费模型请求。

回退：去掉 `?shell=openbot` 并重新加载即可恢复默认入口；重载前应处理当前页草稿与未确认操作，不把重载当成取消或重试。
