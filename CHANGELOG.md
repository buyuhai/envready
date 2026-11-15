# 变更记录

本文件记录用户可见的变化。版本号遵循语义化版本。

在 S4（自更新）落地之前，版本号只用来标记"这一版能跑通什么"，不承诺配置与数据格式的兼容性。

---

## v0.3.0 — 2026-11-15（S2：安装与环境变量）

### 新增

- **`envready install <组件>`**：清单 → 下载 → 校验 → 解压 → 自检 → 写环境变量
  - `--env-root` 指定环境根目录（默认在程序数据目录下）；`--no-env` 只装不写环境；`--force` 覆盖重装
- **安装布局**：`<ENV_ROOT>/<组件>/<版本>/`，随目录写 `.envready-install.json`
  （记录文件、sha256、是否校验、安装时间、镜像）
- **原子落盘**：先解压到同级 `.staging-*`，成功后才改名；自检失败即删除并回滚
- **冒烟验证**：清单可声明 `verify.smoke`——写一个最小程序、编译、运行。
  版本号对不代表能干活（MinGW 在含空格路径下 `gcc --version` 正常但链接失败）
- **环境变量**：
  - Windows：直接读写 `HKCU\Environment`，读原始未展开值、写回保持 `REG_EXPAND_SZ`、
    写前落盘备份、写后回读校验、广播 `WM_SETTINGCHANGE`；PATH 合并幂等
  - Unix：shell 配置里的 `# >>> EnvReady:<组件> >>>` 幂等块，换版本整块替换、卸载整块移除；
    fish 单独用 `set -gx`
- **空格路径策略（数据驱动）**：清单声明 `requirements.space_free_path`，安装前拒绝；
  CLI 会给出建议路径与可直接复制的重试命令

### 实测

- `install python`（python.org 官方 embeddable，10.6 MB）：1.7 秒完成，自检真跑 `python.exe` 解析出 3.12.4
- `install mingw` 到含空格路径：0 秒拒绝、不发起下载，并提示改用 `C:\EnvReady\envs`

### 已知限制

- 只支持 `install_kind=archive`；installer / portable / npm 未实现
- 尚无卸载命令（会在后续版本补）
- MinGW 装在含空格路径下无法链接；开发沙箱只能写 `D:\work place`，因此
  "MinGW 真装 + 编译冒烟"未在本机跑通，冒烟逻辑由注入 Runner 的单测覆盖
- 平台支持：Windows 已有完整实现；macOS / Linux 仅有环境变量写入，安装流程可复用

---

## v0.2.0 — 2026-11-01（S1：清单与下载内核）

### 新增

- **`envready fetch <组件>`**：一条命令完成「读清单 → 选镜像 → 下载 → 校验 → 解压」
  - `--manifest <路径|https地址>` 指定组件清单；`--version` 选版本；`--dest` 指定存放目录；`--extract-to` 下载后解压
  - 位置参数与 flag 可混写（`fetch mingw --manifest x` 与 `fetch --manifest x mingw` 等价）
- **清单**：索引与组件清单解析、schema 版本闸门（过新则拒绝）、字段自洽校验、按显示名/key/别名搜索
- **下载**：多镜像并发测速择优（`GET + Range`，不用 HEAD）、`.part` 断点续传、分层超时（连接 10s / 首字节 30s / 空闲 60s）、边下边算 SHA-256、无 `Content-Length` 时不显示假百分比
- **解压**：按文件头魔术字节识别 `zip / tar / tar.gz / tar.xz / 7z`、`strip_root`、路径穿越与符号链接一律拒绝
- **路径**：三平台目录推导（LOCALAPPDATA / Application Support / XDG）、APP_ROOT 与 ENV_ROOT 严格分离、版本号与 key 的穿越校验

### 实测（真实官方包）

| 包 | 大小 | 校验 | 耗时 | 结果 |
|---|---|---|---|---|
| Python 3.12.4 embeddable（python.org） | 10,565,736 B | SHA-256 一致 | 1.7 s | 解压 35 个文件 |
| MinGW-w64 GCC 16.2.0（winlibs） | 276,414,382 B | 与上游 `.sha256` 旁注一致 | 21 s | 解压 11,874 个文件，`gcc --version` 正常 |

### 已知限制

- 只支持 `install_kind=archive`；`installer` / `portable` / `npm` 在 S2 实现
- 尚未实现：写入环境变量、安装后自检、卸载（S2）
- **MinGW 不能安装在含空格的路径下**：`ld.exe` 不会给内部路径加引号，链接阶段会报 `cannot find D:/work`。
  默认 ENV_ROOT 位于 `%LOCALAPPDATA%` 下，用户名带空格时即触发——S2 必须处理（安装前检查并给出无空格备选路径）

---

## v0.1.0 — 2026-10-18（S0：工程骨架）

- 双入口：`envready`（启动器）与 `envready-gui`（主程序），支持 `--version`
- 三平台 CI：gofmt、go vet、golangci-lint v2.15（含 errcheck）、单元测试、构建、**版本注入断言**
- 14 个 `internal` 分包，每个先写 `doc.go` 明确"负责 / 不负责 / 关键约束"
- 本地脚本：`scripts\dev-shell.cmd`（已配置窗口）、`scripts\build.cmd`（构建并注入版本）
