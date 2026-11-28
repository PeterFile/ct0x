# Openbot 衍生外壳（唯一目标入口）

> **双态边界声明**：`OpenbotShell` 是架构收敛后的**唯一前端外壳目标**。Phase 4 在移除旧群组路由的同一波次中，将彻底移除旧经典兼容布局回退路径与旧默认接待页死代码，并同步接入项目三档策略模板、影子预览卡片、分级对账面板、临时成员交接卡片与 `Verification` 视图。截至基线 `origin/master`（`0188361`），主干当前运行代码仍保留旧经典兼容回退分支与 `Group` 作用域状态接线，直至 Phase 4 合入。

入口执行时只选择一次，只创建一个 root 并调用一次入口渲染；不会监听 URL/history 变化或提供运行中换壳按钮。StrictMode 的开发检查不代表第二次应用启动。默认入口先加载 Openbot JS/CSS 再挂载 App；目标收敛态（Phase 4 落地后）仅保留单壳 `OpenbotShell` 入口并显示中文错误提示，不再维持两套并行布局切换路径。

## 来源与许可

固定来源：[nightly-labs/openbot](https://github.com/nightly-labs/openbot/tree/ac99196e66fe8f9d426433aa5e79d6e999d1c818)，提交 `ac99196e66fe8f9d426433aa5e79d6e999d1c818`。这是实际源码提取后进行的薄 React JSX 适配，不是只模仿配色。

**PolyForm Noncommercial 1.0.0，不是 MIT。** 上游为源码可用的非商业许可，复用须遵守其用途限制；不据此宣称商业用途获准。完整、未修改的 `LICENSE`、`NOTICE` 和固定源文件 SHA-256 清单随 `web/public/licenses/openbot/` 分发；轨道上始终提供“来源与许可”链接（`/licenses/openbot/index.html`，兼容后端只服务普通文件的规则），在新标签页打开以免卸载未完成操作。两条 Required Notice 均完整保留。校验值：

- `LICENSE`：`ffe034ecf735f296ca53cbd856c944849f0c80c7e1490ce162038a99ad89c728`
- `NOTICE`：`3995ad096839d696f4ff70227ac56c89b3c25e6b549bb87cf9d98dce901b75f7`

NOTICE 是保守保留的完整上游包声明，不是此外壳的依赖清单。未复制品牌标志、头像、字体、第三方图标、Blobatar、图表、可选 provider 运行时、Docker seccomp、光标动效或分析代码。`SOURCE.json` 描述只读参考源文件，不代表全部进入分发包。

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

所有外壳样式选择器限定于 `.openbot-shell`。剩余样式段仅连接已有 im-* 槽位与有界滚动区；不改消息展现、滚动 hook 或输入组件。

## 状态与动作边界

- `App.tsx` 仍是选定项目/频道（当前主干未收敛前为旧群组）、pane/context、lazy visits、read activity、focus recovery 的唯一控制器。`AppShellProps` 只传 typed React slots、真实动作及 refs；外壳模块不请求 API，不持有第二套状态，不查询或点击 DOM 代替动作。
- `OpenbotShell` 将页面动作接到 PageRail，复用同一侧边导航（目标 `ProjectChannelNav`）、`Discussion`/composer、`Members`、`Tasks`（含 `Verification` 与依赖钉选视图）、`ChannelPolicyPanel`、`PendingDecisions`、`UnknownReconcilePanel` 与 `HistorySearch`。手机从列表打开详情时，仍由 App 设置 chat pane。轨道“聊天”关闭详情并恢复现有聊天；“列表”关闭详情并恢复当前频道按钮焦点。
- Shell 组件类型与 slot 位置在 pane 或频道切换时稳定；不使用以 pane 或频道为 key 的 shell，不重新挂载 `DiscussionScope` 或 mutation。关闭详情恢复到轨道的实际触发器。隐藏区域保留 DOM，但从键盘与可访问树移除；读取仍由原组件的 active 参数暂停，未完成写操作不会因布局切换 abort 或自动重试。
- 幂等身份与未知结果分类语义不变。原始输入与 key 仅经用户显式同次重试再次发送；界面不把持久化说成 CLI 接收，也不把提交说成验收。
- 不添加依赖、Solid runtime、品牌或第三方资产、假账号/设置/主机/队列操作。Phase 4 完成后，Openbot 外壳成为唯一生产入口。

## 可复现检查

`web/src/openbot-shell/OpenbotShell.test.tsx` 从真实 main 入口载入，检查布局选择与分块渲染约束、每次启动只创建一个 root 并调用一次入口渲染，以及桌面/手机断点与焦点恢复；既有测试继续覆盖分频道草稿、接收者、composer DOM、pending/unknown 消息与任务操作、同次 key/input 重试与状态保持。

运行以下定向检查，保持默认 5s 超时和单 worker：

```sh
cd web
npm test -- --maxWorkers=1 src/openbot-shell/OpenbotShell.test.tsx
npm run typecheck
npm run build
cmp public/licenses/openbot/LICENSE dist/licenses/openbot/LICENSE
cmp public/licenses/openbot/NOTICE dist/licenses/openbot/NOTICE
cmp public/licenses/openbot/SOURCE.json dist/licenses/openbot/SOURCE.json
```

构建结果位于 `web/dist/`。HTML 静态引用共享资源，Openbot JS/CSS 作为应用分块加载。比较体积时分别记录 raw 与同一 Node `gzipSync` 方法下的逐资源 gzip 大小；包体积不代表运行时耗时。来源页面位于 `/licenses/openbot/index.html`，完整法律文件及源码清单从同一目录提供；上述 `cmp` 命令核对分发副本与 public 源文件逐字节一致。

## 产品目标与现有流程的区别

在 Phase 4 前端收敛合入前，现有界面中的手工接收方式控件、消息转任务及手动授权/分派入口属于待淘汰的旧实现事实；若日常讨论到执行仍依赖这些步骤或手动刷新，属于待纠正的产品偏差，而非必须保留的 UX。目标见 [PRD](../PRD.md) 与 [IM interaction contract](orchestration.md#im-interaction-contract)：正常群聊、结构化 `@`/头像右键选人、结合上下文在授权交集内自动准入/分派并回群、三档策略模板与越级影子预览、临时成员交接卡片及两级崩溃对账。
