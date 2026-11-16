# 分派执行回执（最小只读切片）

在现有 OpenBot「请求分派」中读取已知请求状态后，可点击「刷新执行回执」。不新增派发入口、轮询、Review 历史或导航。重新加载页面后，仍通过原有「已知分派请求标识」入口恢复查询；不能按任务发现请求，也不能用查询结果确认一次结果未知的 POST 或替换其幂等键。

## 本地用户接口

`GET /api/v1/groups/{group}/tasks/{task}/dispatch-requests/{request}/execution`

只随现有 `DispatchRequestRoutes` 注册，沿用角色运行时的 opt-in 路由开关；不开放成员工具或模型自选身份。所有标识精确匹配，不接受查询参数。群组或任务与请求不匹配返回 404，不返回部分执行信息。缺少路由也返回 404。

沿用现有本地 HTTP 传输边界：Host 校验适用于 GET；Origin/Fetch Metadata 的写入限制不扩展到只读 GET，也不增加跨源 CORS 授权。客户端使用 `redirect: "error"`、`cache: "no-store"` 和取消信号。

## 安全 DTO

顶层字段固定为：

- `group_id`、`task_id`、`request_id`：查询的精确身份。
- `dispatch_status`：实际协调状态，不是执行状态。
- `coordinator_turn`：物化投递已被认领且关系有效时的协调员轮次，否则为 `null`。
- `decision`：实际决定的 `id`、`kind`、`coordinator_turn_id`、`created_at`，否则为 `null`；不返回理由。
- `assignment`：实际分配的 `id`、`request_id`、`decision_id`、`task_id`、`group_id`、`worker_member_id`、`created_at`，否则为 `null`。
- `assignment_dispatch_status`：实际分配投递状态，否则为 `null`。
- `binding`：实际分配/任务尝试绑定的 `assignment_id`、`group_id`、`task_id`、`worker_member_id`、`turn_id`、`attempt_id`、`task_revision`、`created_at`，否则为 `null`。
- `worker_turn`：存在精确关联的已认领成员轮次时返回，即使尚未绑定任务尝试；否则为 `null`。

两个 Turn 均只包含 `id`、`group_id`、`delivery_id`、`conversation_id`、`member_id`、`state`、`native_session_id`、`accepted_at`、`cancel_requested`、`cancel_requested_at`、`created_at`、`updated_at`、`receipt`。取消标志表示持久取消请求是否存在，不表示已经停止。

`receipt` 从持久化、适配器已归一化的 `turns.receipt_json` 投影；不存在时为 `null`。唯一白名单为：

```text
started accepted outcome terminal delivery_failed native_session_id
 direct_child_reaped output_complete exit_code logs_truncated
```

空 `terminal` 表示未观测到原生终态。回执不包含 raw receipt、reason、日志路径、提示词、候选输出、token 或 claim_token。读取不访问日志文件、不调用模型或提供商。

## 证据与状态的含义

- 请求登记、决定分配、排队投递、任务尝试绑定都不能推导出正在执行。
- Turn 按持久状态展示 `starting`、`running`、`cancel_requested`、`unknown`、`succeeded`、`failed`、`aborted`、`truncated`、`not_started`，不从空回执推断成功或未执行。
- `started`/`accepted` 是该次调用的观测；未知结果时的 `false` 不否定既往执行。CLI 接收不是任务交付或用户验收。
- `outcome=unknown` 即使保留 `terminal=succeeded`，也仍是未知结果；原生终态不覆盖归一化结果。
- `output_complete` 表示直接子进程/输出完成证据；`direct_child_reaped` 只说明直接子进程已回收，不证明后代进程或外部工具停止。日志截断与模型输出被截断分别展示。
- 所有展示都是显式刷新时的样本。隐藏组件、切换群组/任务/请求或刷新失败会移除旧样本；effect 同时 abort 和 ignore 迟到响应。执行成功不代替 Task 提交，更不代替独立的用户 Review。

## 读取与验证边界

Store 在单个只读事务内复用请求、决定、分配/绑定的现有关系校验；分配读取仅跳过无关的 Task/依赖列表投影，原 `GetAssignment` 行为不变。读取按已知主键及投递身份进行，不扫描历史列表；校验消息发送者、群组/话题、成员、会话、双向 delivery/Turn 及绑定关系。任何失败均不返回部分 DTO。

此路径不会物化工作、登记尝试、取消或对账执行、变更未知状态、释放资源、自动提交或验收。聚焦测试覆盖 HTTP 作用域与传输安全、全状态回执白名单、取消/未知资源与数据不变、数据库重开、单事务快照，以及前端失效样本和迟到响应隔离；不代替另行执行的真实 Sol 运行验收。
