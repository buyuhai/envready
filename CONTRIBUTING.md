# 贡献与发布规范

本文件规定两件事：**提交怎么写**、**版本怎么进**。两件事都由 CI 兜底，不靠记性。

---

## 一、提交信息

格式：

```
<type>(<scope>): <一句话说明>
```

- **type**（必填，只能取这些）：
  `feat` 新能力 / `fix` 修复 / `docs` 文档 / `chore` 杂务 / `ci` 流水线 /
  `refactor` 重构 / `test` 测试 / `perf` 性能 / `build` 构建
- **scope**（建议填）：包名或区域，如 `paths`、`download`、`archive`、`install`、`envvar`、
  `app`、`cmd`、`manifest`、`repo`、`ci`、`notes`
- **说明**：用中文，写"做了什么"；正文写**为什么**，不要复述 diff。

正确示例：

```
feat(download): 支持服务端忽略 Range 时自动从头重下
fix(archive): strip_root 对顶层目录条目误判
chore(release): v0.3.0（S2 完成）
```

禁止：空描述、`update`、`fix bug`、`修改`、把多个不相关的改动塞进一个提交。

---

## 二、版本号

语义化版本 `vX.Y.Z`，用**附注标签**（`git tag -a`），不用轻量标签。

| 位 | 何时递增 | 例子 |
| --- | --- | --- |
| **MAJOR** | 破坏性变更：清单格式不兼容、CLI 参数移除、数据目录结构变更 | v1.0.0 之前的破坏性变更也照样跳 |
| **MINOR** | 新增阶段能力（每个 S 阶段收口一个） | S2 完成 → v0.3.0 |
| **PATCH** | 修复、文档、内部整理 | 修一个 bug → v0.3.1 |

当前版本见 [README.md](README.md)，每个版本的变更见 [CHANGELOG.md](CHANGELOG.md)。

> S4（自更新）落地前，版本号只标记"这一版能跑通什么"，不承诺配置与数据格式兼容。

---

## 三、发布流程（四步，缺一不可）

1. 更新 `CHANGELOG.md`：新增能力 / 实测数据 / 已知限制
2. 更新 `README.md`：阶段表与"当前版本"
3. 提交：`chore(release): vX.Y.Z（<阶段> 完成）`
4. 打附注标签并推送，然后建 GitHub Release（说明与 CHANGELOG 保持一致）：

```bash
git tag -a vX.Y.Z -m "vX.Y.Z — <一句话>"
git push origin main
git push origin vX.Y.Z
```

---

## 四、历史不可改写（硬规则）

- **已推送的 `main` 只允许追加新提交**：不 force push、不 amend、不对已推送提交做 rebase。
- 发现错误就**再提交一次修正**，把演进过程留在历史里——这比"看起来一直很完美"更有价值。
- 每次推送后必须核对远端，不依赖本地 `origin/main` 追踪引用：

```bash
git ls-remote origin refs/heads/main   # 必须等于本地 HEAD
# 推送走的是带 token 的 URL，git 不会更新追踪引用，需手动校准：
git update-ref refs/remotes/origin/main HEAD
```

---

## 五、CI 会检查什么

除三道常规门禁（gofmt / vet+golangci-lint / test）外，还有：

| 检查 | 作用 |
| --- | --- |
| 提交信息格式 | 每次推送引入的提交必须符合上面的 `<type>(<scope>): 说明` |
| 版本注入断言 | 用 `-ldflags` 构建真程序，断言版本号与提交号出现（防止 `-X` 写错而 CI 假绿） |
| 注册表集成测试跳过即失败 | 仅 Windows 执行；出现 `SKIP` 直接判失败（`go test` 里 skip 与 pass 都显示 ok） |
