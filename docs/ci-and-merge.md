# PR 检查、合并和发布

## 推送和选择检查

向开放 PR 的来源分支推送新提交，会更新该 PR 的检查。草稿和待审 PR 默认按改动路径检查。没有开放 PR 的普通分支推送无独立 CI。推送到 `master` 会执行完整套件。

- 普通 PR 沿用按路径选择的 Go、Web、Pi bridge 或 CI 脚本检查。仅文档变更只检查 Git 差异。
- 添加 `ci:full` 可在 PR 上执行完整套件。标签存在期间，后续相关事件继续执行完整套件。
- 添加 `ci:refresh` 会重跑当前层级。再次触发时，先移除再添加该标签。移除 `ci:full` 后，下次相关事件恢复按路径检查。
- 同一 PR 的相关新运行会取消旧运行。标题、正文和其他标签的编辑不会取消检查。连续推送到 `master` 时，正在运行的检查会完成，新排队的运行会替换此前等待的运行。

## 合并已审查的提交

1. 记录独立审查通过的 PR head SHA。在隔离工作树中验证该 head 合入最新 `origin/master` 的结果，记录目标分支 SHA、合并树和已运行的检查。
2. 合并前核对 head 和 `master`。head 变化时审查新差异并重跑相关检查。若 `master` 前移，更新合并树并重跑相关检查。规则集 `27715395` 要求 `Full CI` 时，先使用 `ci:full` 并等对应检查通过。
3. 将 PR 编号存入 `PR`，将已审查的完整 head SHA 存入 `REVIEWED_HEAD`，再执行合并。

   ```bash
   gh pr merge "$PR" --merge --match-head-commit "$REVIEWED_HEAD"
   ```

## 切换主干规则

1. 给 CI 改造 PR 加 `ci:full`，按现有规则通过 `Full CI` 后合并。
2. 确认合入提交在 `master` 上的推送运行通过 `Full CI`。
3. 执行已授权的规则切换。将规则集 `27715395` 的必需状态检查替换为禁止删除和非快进更新。之后普通 PR 按上述审查和验证流程合并，完整套件仍可用 `ci:full` 请求。

要恢复原规则时，先合并恢复默认全量调度的回滚 PR，再将 `Full CI` 设回必需检查。

## 发布已通过完整套件的提交

发布前核对提交 SHA，确认该 SHA 对应的 `master` 推送运行已通过 `Full CI`。这次运行应包含持久化、重试、跨群组隔离、资源锁和未知执行结果的发布检查。

主干并发策略和指定 head 合并参考 Magpie。[测试工作流](https://github.com/yetone/magpie/blob/main/.github/workflows/test.yml) · [合并与发布约定](https://github.com/yetone/magpie/blob/main/docs/code-standards.md)
