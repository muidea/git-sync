# GitHub 仓库定期镜像备份工具系统设计文档 (v2.0)

## 1. 背景与设计目标

### 1.1 项目背景
研发团队或个人开发者将代码资产托管于 GitHub 云端时，面临网络连通性波动、账号凭证异常受限、人为误操作（如强推覆盖分支、误删仓库）以及合规灾备等风险。实际生产环境中，团队通常只需要对**明确指定的核心仓库清单**进行定期灾备，避免自动扫描拉入大量实验性、临时性或无需备份的仓库占用带宽与存储；同时，运维人员需要随时、直观地确认备份任务的同步健康度与数据一致性。因此，亟需一套**纯白名单驱动**、支持**严格引用镜像对齐**且具备**完善状态可观测性**的本地自动化备份工具。

### 1.2 核心设计目标
1. **显式白名单精准管控**：采用声明式精确仓库清单（`owner/repo`）作为唯一同步来源，不做组织/账号全量自动发现；支持单库级参数覆盖（如独立超时、LFS 开关、PR 引用开关），按需拉取单库元数据，最大化节省 API 配额与存储资源。
2. **严格引用一致性**：本地采用裸仓库镜像模式，完整保留远端分支（Branches）、标签（Tags）、Git Notes 及可选的 Pull Request 引用（`refs/pull/*`），在远端发生 `rebase`、`push --force` 或分支删除时保持本地与远端严格对齐。
3. **只增不删防连带损毁**：严格区分“从白名单移除（`unmanaged`）”与“白名单内远端不存在或权限丢失（`missing`）”；**当白名单中的仓库在远端不存在（404）时，本地已有备份一律原地保留，绝不自动删除或移出活跃目录**，仅标记异常状态并触发告警通知。
4. **全链路状态可观测与可验证**：无需引入外部数据库或常驻服务，通过原子状态账本、内置 CLI 状态诊断与远端引用一致性比对命令（`status` / `verify`）、Prometheus 指标暴露及异常 Webhook 告警，闭环掌控同步状态。
5. **零明文密钥残留与极简运维**：认证凭证仅在运行期通过进程环境变量动态注入，严禁写入本地 Git 配置文件或命令行参数；单二进制无状态 CLI 设计，天然契合 `systemd timer` 或 `cron` 调度。

> **平台与备份范围边界**：
> * **托管平台**：当前仅支持 **`github.com`**（暂不适配 GitHub Enterprise Server 或其他第三方 Git 平台）。
> * **数据范围**：专注于备份 GitHub 仓库的 **Git 对象库、完整引用树及 Git LFS 大文件对象**，不包含 GitHub Wiki（`.wiki.git`）、Issues / PR 评论区数据、Release 二进制附件、Actions Secrets 及仓库权限配置，亦不递归拉取指向外部第三方的 Git Submodule。

---

## 2. 系统总体架构

### 2.1 架构分层与技术基线
工具以 **Go** 编译为单二进制无状态 CLI，依赖宿主机 `git >= 2.31`（支持 `GIT_CONFIG_COUNT` 环境变量凭证注入）及 `git-lfs`（可选，开启 LFS 时需要），启动时自动校验运行时依赖版本。系统采用自顶向下的六层模块化架构设计：

1. **CLI 命令接入与调度层**：提供统一命令行入口，划分 `sync`（定时同步主流程）、`targets`（白名单配置预检）、`status`（同步状态查询）、`verify`（远端/本地强一致性深度校验）与 `report`（周期健康度周报推送）五大子命令；同步入口通过操作系统文件排他锁（`flock`）保障全局单实例运行。
2. **配置解析与安全认证层**：解析声明式 `config.yaml` 与环境变量，校验白名单唯一性与单库覆盖策略，管理 Fine-grained PAT 或 GitHub App Installation Token（含过期前自动续期），并通过 Git 环境变量机制完成无落盘凭证注入。
3. **白名单元数据轻量查询层**：按白名单列表数量自动选择最优查询路径（小规模走带 `ETag` 的单库 REST 接口；大规模走分批 GraphQL 节点查询），精准获取每个目标仓库的不可变 `repo_id`、规范 `full_name`、`pushed_at` 与可达性状态。
4. **差集对账与容灾决策层**：对比“配置白名单 + 远端元数据”、“本地持久化账本（`repo_ledger.json`）”与“物理目录树”，精准识别“新增克隆”、“增量更新”、“无变更跳过”、“重命名迁移”、“同路径 ID 冲突隔离”、“移出白名单失管（`unmanaged`）”及“远端不存在/失权（`missing`）”七种状态。
5. **并发同步执行引擎层**：维护有界 Worker Pool，管控并发度、分级超时、临时目录原子克隆（`.tmp_clones`）与指数退避重试，驱动底层 `git`、`git-lfs` 与周期性 `git maintenance` 子进程。
6. **状态持久化与可观测告警层**：采用临时文件原子替换机制更新全局运行摘要与仓库级账本，输出结构化日志、导出 `node_exporter` 监控指标文件，并在异常或恢复时触发边缘防抖 Webhook 告警。

### 2.2 核心业务工作流（`sync` 主流程）

```text
[定时调度启动 git-backup sync]
      │
      ▼
[获取全局排他锁 .backup.lock] ──(锁被占用)──> [打印提示并以退出码 0 安全退出]
      │
      ▼
[校验 git 版本 / 磁盘空间 / 加载配置 / 初始化认证器] ──(致命异常)──> [高优告警并以退出码 1 中止]
      │
      ▼
[清理 .tmp_clones 残留] ──> [白名单元数据轻量查询 (github.com)]
                              ├── 白名单数 <= 20 ──> 并发单库 GET /repos/{owner}/{repo} (带 ETag)
                              └── 白名单数 >  20 ──> 分批 GraphQL 节点批量查询 (每批 <= 50 节点)
      │
      ▼
[三方差集对账: 配置白名单 vs 本地 repo_ledger.json vs 磁盘目录]
  ├── A. 已从配置白名单移除 (Unmanaged) ──> 默认原地保留(retain)或归档(archive)
  ├── B. 白名单内远端 404 (Missing)     ──> 本地已有备份原地保留，标记 missing 并触发告警
  ├── C. 同 repo_id 但 full_name 变更   ──> 本地原子 mv 目录 + 更新 remote url -> 转入增量同步
  ├── D. 同路径但 repo_id 冲突          ──> 将旧目录隔离至 .trash -> 执行新库克隆
  ├── E. pushed_at 未变且健康未超期     ──> 标记 up_to_date，跳过 Git 网络通信与磁盘扫描
  └── F. 新增、历史失败或有推送更新     ──> 投递至并发 Worker Pool
                                              │
                                              ▼
                                  ┌─────────────────────────────────┐
                                  │ 1. 首次: .tmp_clones 克隆后原子 mv │
                                  │ 2. 增量: 校验 refspec + update  │
                                  │ 3. 扩展: lfs fetch --all        │
                                  │ 4. 采集: HEAD SHA & 磁盘量      │
                                  │ 5. 维护: 按周期触发 GC          │
                                  └─────────────────────────────────┘
                                              │
                                              ▼
[清理超期 .trash 目录并回收对应账本条目] ──> [原子写入账本 & status_summary.json & git_backup.prom]
      │
      ▼
[按边缘触发+冷却策略推送告警/恢复通知] ──> [按状态返回分级退出码 0/1/2]
```

---

## 3. 核心功能模块矩阵与详细技术方案

### 3.1 功能项全景对照表

| 模块分类 | 功能项名称 | 核心技术方案摘要 |
| :--- | :--- | :--- |
| **白名单管控** | 显式白名单与覆盖 | 仅备份 `targets` 中显式声明的 `owner/repo`，支持单库覆盖 `enable_lfs`、超时、`mirror_pr_refs` 等参数 |
| **白名单管控** | 轻量元数据路由 | $\le 20$ 仓库并发调用单库 REST（配 `If-None-Match` 省配额）；$> 20$ 仓库自动分批 GraphQL 查询 |
| **白名单管控** | 变更预检（Dry-Run） | `targets --check` 校验白名单格式与重复项、探测远端可达性/重命名及待失管仓库，不修改本地目录 |
| **安全认证** | 环境变量无落盘注入 | 通过 `GIT_CONFIG_COUNT/KEY/VALUE` 环境变量注入认证头，杜绝写入 `.git/config` 与暴露在命令行参数中 |
| **安全认证** | 凭证自动续期与防挂起 | GitHub App Token 剩余有效期 $< 10\text{m}$ 自动续期；强制 `GIT_TERMINAL_PROMPT=0` 防子进程挂起 |
| **核心同步** | 原子镜像初始化 | 先在 `.tmp_clones` 执行 `git clone --mirror`（或定制 refspec），成功后原子移入 `repos/` 防半成品残留 |
| **核心同步** | 严格一致增量对齐 | 校验配置 refspec 变更后执行 `git -C <dir> remote update --prune`，强推对齐、删引用清理 |
| **核心同步** | 增量跳过与周期强校 | 基于 `pushed_at` 跳过无变更且健康仓库（历史失败库禁止跳过）；每隔 $D$ 天强制同步校准一次 |
| **扩展同步** | Git LFS 对象备份 | 启用 LFS 或探测到 LFS 特征时追加 `git lfs fetch --all`，保障二进制大文件完整 |
| **容灾对账** | 失管与缺失对账解耦 | 区分“移出配置（`unmanaged`）”与“远端 404（`missing`）”；**`missing` 仓库原地保留本地备份并告警** |
| **容灾对账** | 重命名迁移与冲突隔离 | 基于 `repo_id` 识别改名（含大小写变更）并原地迁移；同路径不同 `repo_id` 先隔离旧目录至 `.trash` |
| **容灾对账** | 批量异常告警保护 | 当 `missing` 占比超 `max(2, ceil(N * max_missing_alert_ratio))` 时触发高优排查告警 |
| **并发与存储** | 限流重试与自动 GC | 有界 Worker Pool + 指数退避重试；按同步频次触发 `git maintenance run --auto`；基于 `mtime` 清理陈旧锁 |
| **状态观测** | 双层原子状态账本 | 原子落盘 `status_summary.json` 与 `repo_ledger.json`，跳过库直接复用历史磁盘占用统计以节省 I/O |
| **状态观测** | 终端查询与强一致校验 | 内置 `status` 诊断表格，以及剔除 `HEAD`/`^{}` 后的 `verify` 实时 `ls-remote` vs `show-ref` 一致性校验 |
| **监控告警** | 分级退出码与外部推送 | 划分 `0/1/2` 退出码；导出 Prometheus 指标；边缘触发 + 冷却防抖 + 恢复通知；`report --weekly` 周报 |

---

### 3.2 各功能模块详细设计

#### 模块一：显式白名单管理与轻量元数据查询

1. **纯白名单约束、大小写规范化与 ID 去重**：
   * 工具仅认可 `config.yaml` 中 `targets` 列表显式声明的 `owner/repo`。启动解析阶段即进行格式校验与大小写不敏感去重，存在重复项则报配置错误（退出码 `1`）。
   * 若因仓库改名过渡期导致配置中同时存在旧名与新名且远端解析后指向同一个 `repo_id`，直接报配置冲突错误（退出码 `1`），防止并发 Worker 竞争同一物理目录。
   * 本地目录路径与账本记录统一以 **GitHub API 返回的规范大小写 `full_name`** 为准，避免 Linux 大小写敏感文件系统与配置拼写不一致。
   * 全局 `defaults` 提供基础同步策略，任意白名单条目均可配置独立覆盖参数（如 `enable_lfs`、`clone_timeout`、`update_timeout`、`skip_unchanged`、`mirror_pr_refs`）。
2. **轻量 API 元数据路由（`api.github.com`）**：
   * **小规模路径（目标数 $\le 20$）**：并发请求 `GET https://api.github.com/repos/{owner}/{repo}`（携带 `Accept: application/vnd.github+json` 与 `X-GitHub-Api-Version: 2022-11-28`），并附带 `repo_ledger.json` 缓存的 `If-None-Match: "<etag>"`。返回 `304 Not Modified` 不扣减 REST 速率配额；若远端仓库发生改名，REST 接口会自动跟随 `301` 重定向并返回包含原 `id` 与新 `full_name` 的对象。
   * **批量路径（目标数 $> 20$）**：分批拼装为 **GitHub GraphQL API** 查询（每批 $\le 50$ 个 `repository(owner, name)` 别名节点），单次请求批量获取 `databaseId`、`nameWithOwner`、`pushedAt`、`isEmpty` 与 `defaultBranchRef`。
   * **GraphQL 局部错误严格判定**：GraphQL 即使内部节点异常也常返回 HTTP `200 OK`。系统必须逐节点解析响应体 `errors[]`：仅当错误类型为 `NOT_FOUND` 时才将该仓库标记为远端不存在（`missing`）；若出现 `FORBIDDEN`、`RATE_LIMITED` 或其他瞬态错误，则视为本轮查询失败（按 `failed` 记入或触发重试），**严禁误判为 `missing`**。
3. **白名单预检机制（`targets --check`）**：
   * 管理员增删 `config.yaml` 白名单后，可执行预检命令：验证配置格式、探测所有目标仓库在 `github.com` 的可达性与当前 `repo_id`（若白名单配置名已发生远端改名重定向，提示修正配置文件），并列出本地账本中将被标记为 `unmanaged` 的仓库清单，不修改磁盘数据。

#### 模块二：安全认证与无落盘凭证注入方案

1. **最小权限凭证选型**：
   * 个人场景采用 **Fine-grained PAT**，仅授予白名单目标仓库的 `Metadata: Read-only` 与 `Contents: Read-only` 权限。
   * 企业组织场景优先采用 **GitHub App**：配置 `app_id`、`installation_id` 与私钥路径，使用私钥签发 JWT 换取有效期 $1$ 小时的 `Installation Access Token`。
   * **长任务 Token 自动续期**：在内存中维护并发安全的 Token Provider。由于大仓库克隆超时可达数小时，每次调度启动子进程（`clone` / `remote update` / `lfs fetch` / `ls-remote`）前检查 Token 剩余有效期，若不足 $10$ 分钟则自动重新签发。
2. **环境变量动态注入（杜绝命令行参数泄露）**：
   * 统一使用不含用户名密码的纯净 HTTPS 地址 `https://github.com/<owner>/<repo>.git`。
   * **禁止使用 `git -c ...`**（命令行参数会暴露在 `/proc/<pid>/cmdline` 与 `ps` 进程列表中）。通过 Git 2.31+ 原生环境变量注入认证头：
     ```text
     GIT_CONFIG_COUNT=1
     GIT_CONFIG_KEY_0=http.https://github.com/.extraheader
     GIT_CONFIG_VALUE_0=AUTHORIZATION: basic <BASE64("x-access-token:" + token)>
     GIT_TERMINAL_PROMPT=0
     GCM_INTERACTIVE=never
     ```
   * 仅对 `https://github.com/` 域生效的 `extraheader` 可避免 `git-lfs` 向预签名对象存储（如 S3/CDN）下载大文件时误带 GitHub 认证头导致 `400` 报错。
   * 若配置了 `network.proxy_url`，统一通过子进程环境变量 `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY` 透传给 `git` 与 `git-lfs`，并在日志脱敏器中注册正则过滤 Base64 密钥与代理 URL 中的认证段。

#### 模块三：Git 裸仓库镜像与 LFS 同步方案

1. **临时目录原子克隆（防中断半成品残留）**：
   * 首次克隆不直接写入 `repos/`，而是先克隆至同文件系统挂载点下的 `<root>/.tmp_clones/<owner>/<repo>.git`，完整成功后再通过 `os.Rename()` 原子移入 `<root>/repos/<owner>/<repo>.git`。每次 `sync` 启动时自动清理 `.tmp_clones/` 中因上次崩溃/OOM 遗留的无效目录。
   * 默认执行 `git clone --mirror`，自动建立 `remote.origin.fetch = +refs/*:refs/*` 映射。
   * **PR 引用开关动态同步**：当配置 `mirror_pr_refs: false` 时，初始化使用 `git clone --bare` 并将 `remote.origin.fetch` 显式设为 `+refs/heads/*:refs/heads/*`、`+refs/tags/*:refs/tags/*` 及 `+refs/notes/*:refs/notes/*`。对于存量仓库，每次增量同步前对比当前 `remote.origin.fetch` 与生效配置；若 `mirror_pr_refs` 发生变更，自动重写 refspec 后执行 `remote update --prune` 以清理或补全 `refs/pull/*`。
2. **定期严格增量同步**：
   * 命令：`git -C <target_dir> remote update --prune`
   * `+` 强制引用更新语义确保远端发生 `rebase` 或 `push --force` 改写历史时，本地直接无冲突对齐最新指针；`--prune` 自动清除远端已删除的分支与标签。
3. **增量预判、周期强校与安全防跳过**：
   * 仅当同时满足以下 **全部条件** 时，才允许跳过网络同步并标记 `up_to_date`：
     1. `skip_unchanged: true`；
     2. 本地目录存在且上次状态健康（`consecutive_failures == 0` 且上一轮状态为 `updated`、`cloned` 或 `up_to_date`）；
     3. 远端 `pushed_at` 与账本中记录的 `remote_pushed_at` 完全一致；
     4. 距离 `last_git_sync_at` 未超过 `force_full_sync_days`（默认 $7$ 天）。
   * **严格写入顺序**：仅当 Git 与 LFS 同步均执行成功后，才将最新的 `remote_pushed_at` 写入 `repo_ledger.json`，防止失败后下一轮被误跳。
4. **Git LFS 二进制扩展同步**：
   * 裸仓库无工作区文件，LFS 触发条件判定为：白名单生效策略 `enable_lfs: true`；或当配置为 `auto` 时，通过 `git -C <target_dir> show HEAD:.gitattributes`（及已存在本地 `lfs/` 缓存目录，或在首次克隆后执行一次 `git -C <target_dir> lfs ls-files --all`）判定是否含 LFS 对象。
   * 若仓库触发 LFS 同步但宿主机未安装 `git-lfs` 二进制，**直接将该仓库标记为 `failed` 并告警**，杜绝静默跳过导致大文件缺失。
   * 满足条件时在 `remote update` 完成后追加执行 `git -C <target_dir> lfs fetch --all`（继承相同的环境变量认证与代理配置）。为遵循灾备“只增不删”原则，**不执行 `git lfs prune`**，永久保留历史已拉取的 LFS 对象。

#### 模块四：对账解耦（失管 vs 远端不存在）与本地保全方案

纯白名单模式下，状态归因清晰确定，系统坚持**“已有备份宁留勿删”**原则：

1. **状态 A：移出白名单失管（`unmanaged`）**：
   * **判定逻辑**：某仓库存在于 `repo_ledger.json` 中，但其 `repo_id` 与 `full_name` 均已不再命中当前 `config.yaml` 的 `targets` 白名单（管理员主动移出配置）。
   * **处置策略**：不再请求其远端 API。按 `unmanaged_policy` 执行：
     * `retain`（推荐默认）：保留本地裸仓库原位不动，账本标记 `state: "unmanaged"`，停止同步并在 `status` 中提示。
     * `archive`：移入 `.trash/unmanaged_<timestamp>/<owner>/<repo>.git`，进入归档保留周期。
2. **状态 B：白名单内远端不存在或权限丢失（`missing`）**：
   * **判定逻辑**：仓库依然存在于当前 `config.yaml` 白名单中，但 `github.com` API 明确返回 `404 Not Found`（REST 404 或 GraphQL `NOT_FOUND`——包括远端真删库，或 Fine-grained PAT / GitHub App 被收回了该私有库的访问权限）。
   * **处置策略（本地保留，绝不删移）**：
     * **若本地已存在该仓库的历史备份目录**：**原地保留在 `repos/<owner>/<repo>.git` 不动，严禁删除或移入 `.trash`**；仅在 `repo_ledger.json` 中更新 `state: "missing"` 并停止后续 Git 拉取，同时触发告警通知运维核查（若后续该库恢复可达且 `repo_id` 一致，下轮同步自动无缝恢复增量更新）。
     * **若本地尚无任何历史备份（首次加入白名单即 404）**：仅在账本/摘要中记录 `missing` 并告警。
   * **批量 404 高优告警**：若单轮 `missing` 数量满足 $N_{\text{missing}} \ge \max\left(2,\ \lceil N_{\text{managed}} \times \text{max\_missing\_alert\_ratio} \rceil\right)$（默认 $30\%$），立即推送高优紧急告警提示检查 Token 权限或组织变动。
3. **重命名迁移、存量接管与同路径 ID 冲突处理**：
   * **同 ID 改名迁移**（含仅大小写变更）：当白名单中的仓库返回的 `repo_id` 与本地账本中已有记录一致但 `full_name` 不同时，将本地目录原子迁移至新 `full_name` 对应路径并更新 `origin` 地址，随后转入增量同步；若靠 REST `301` 发现改名而配置文件尚未同步修改，在日志与 `status` 中提示更新 `config.yaml`。
   * **存量目录冷启动接管（Adopt）**：若磁盘目标路径已存在合法裸仓库（`git rev-parse --is-bare-repository` 为 `true` 且 `remote.origin.url` 指向同一 GitHub 仓库）且 `repo_ledger.json` 中尚无该目录对应的 `repo_id` 记录（如旧备份迁入或账本重建），直接将其接管录入账本并转入增量 `remote update`，避免无谓重拉大仓库。
   * **同路径不同 ID 冲突**（如远端删库后重建同名新库，或先前 `retain` 失管的同名旧目录尚在原位）：在从 `.tmp_clones` 移入 `repos/` 前检测目标路径是否已被账本记录的**另一个 `repo_id`** 占用；若冲突，先将旧目录安全移入 `.trash/conflict_<timestamp>/` 隔离（保留 $30$ 天），再将新库落位。
   * **归档清理与账本回收**：仅 `unmanaged_policy: "archive"` 与 `conflict_*` 目录会进入 `.trash/`，驻留满 `trash_retention_days`（默认 $30$ 天）后由清理例程物理删除，并同步清理 `repo_ledger.json` 中对应的已回收条目。

#### 模块五：并发控制、网络容错与本地存储自愈

1. **并发限流与分级超时**：
   * 采用有界 Worker Pool（默认并发 $N = 6$），避免触发 GitHub Secondary Rate Limits。
   * 区分首次克隆超时（如 `clone_timeout: 1h`）与日常增量更新超时（如 `update_timeout: 15m`），支持在单库白名单项中覆盖。
2. **指数退避重试与 API 限流退避**：
   * 针对网络瞬断（TLS 握手超时、连接重置、`502/503/504`）执行自动重试，第 $k$ 次重试（$k \in \{1, 2, \dots, K_{\max}\}$）等待时长为：
     $$t_k = \min\left(t_{\max}, t_0 \cdot 2^{k-1}\right) + \Delta t$$
     其中 $t_0 = 2\text{s}$，$t_{\max} = 30\text{s}$，随机抖动 $\Delta t \in [0, 1000\text{ms}]$。
   * 遇到 GitHub Secondary Rate Limit（响应头含 `Retry-After`）且等待时间 $\le 60\text{s}$ 时按指定秒数休眠后重试；若遇到 Primary Rate Limit（`X-RateLimit-Remaining: 0`）或 `Retry-After > 60s`，不进行长时间阻塞硬等，立即以退出码 `1` 快速失败并输出预计配额重置时间。
3. **周期性存储压缩与陈旧锁清理**：
   * 账本记录 `sync_count_since_gc`，每累计实际拉取更新满 $M$ 次（默认 $M = 25$），执行 `git -C <target_dir> maintenance run --auto`，打包松散对象并更新 `commit-graph`。
   * 若遇异常断电残留 Git 内部 `.lock` 文件（如 `packed-refs.lock`、`shallow.lock`）导致更新失败：在确认持有全局 `.backup.lock` 且残余锁文件 `mtime` 距今超过 $1$ 小时后，安全移除陈旧 `.lock` 并执行带保护超时的 `git -C <target_dir> fsck --no-dangling` 校验（超时不阻塞主流程，仅标为该库失败并告警）。

#### 模块六：同步状态可观测性、深度校验与监控告警方案

1. **双层状态文件原子持久化与统一状态机**：
   * 统一仓库状态枚举：`cloned`（本轮新克隆）、`updated`（本轮有更新同步）、`up_to_date`（无变更跳过）、`failed`（重试后仍失败）、`missing`（白名单内远端 404/失权，本地已保留）、`unmanaged`（移出白名单失管）。
   * **汇总口径统一**：`status_summary.json` 中 `managed_total` 等于当前白名单仓库总数（即 `cloned + updated + up_to_date + failed + missing`），`unmanaged` 为独立统计的存量失管数。
   * 每次 `sync` 结束先写入 `.tmp` 临时文件并 `fsync()`，再通过 `os.Rename()` 原子替换 `status_summary.json` 与 `repo_ledger.json`。
   * **磁盘占用统计优化**：仅对本轮实际执行过 `clone`、`remote update`、`lfs fetch` 或 `gc` 的仓库重算 `disk_size_bytes`；`up_to_date`、`missing` 等未变更目录直接复用账本缓存值，节省 NAS/HDD I/O。
2. **CLI 状态查询（`status`）与强一致性校验（`verify`）**：
   * **`git-backup status`**：毫秒级读取本地账本并输出对齐表格，默认置顶高亮 `failed` 与 `missing`；支持 `--failed`、`--stale 168h`、`--json` 及 `--root <dir>`（支持无配置文件直接查询）。
   * **`git-backup verify [owner/repo]`**：全量模式下自动跳过 `missing` 与 `unmanaged` 仓库，仅对当前可达的白名单目标仓库并发校验；支持 `--json` 输出结果。复用 Worker Pool 并发与环境变量认证机制，向 `github.com` 发起实时 `git ls-remote <url>` 并与本地 `git -C <dir> show-ref` 做字典比对（空仓库两者均为空则判定通过）。比对前剔除 `ls-remote` 中的 `HEAD` 行、所有 `^{}`（Annotated Tag 解引用行）以及动态重算的 `refs/pull/*/merge`，确保分支、轻量 Tag、附注 Tag 对象与 `refs/pull/*/head` 校验结果 $100\%$ 精确无误报；附加 `--fsck` 时同步校验本地 Git 对象库完整性。
3. **分级退出码、Prometheus 指标与 Webhook 推送**：
   * **标准进程退出码**：
     * `0`：本轮所有白名单目标仓库同步或校验全部成功（含无变更跳过）。
     * `1`：全局致命失败（配置错误、依赖版本不满足、认证 401/403、API 配额耗尽、磁盘空间不足）。
     * `2`：部分仓库异常（主流程跑完，但存在 $\ge 1$ 个白名单仓库同步 `failed` 或远端 `missing`）。
   * **Prometheus Textfile 指标**：原子写出 `git_backup_last_run_timestamp_seconds`、`git_backup_duration_seconds`、`git_backup_repos_total{status="cloned|updated|up_to_date|failed|missing|unmanaged"}` 及 `git_backup_storage_bytes_total`。
   * **边缘防抖 Webhook 告警与周报**：
     * 支持可选 `webhook_secret_env`（用于飞书 / 钉钉 HMAC-SHA256 加签校验）；`generic` 类型发送包含 `event_type`、`summary`、`abnormal_repos` 字段的标准 JSON POST 请求。
     * 单库 `consecutive_failures` 达到 `failure_threshold`（默认 `2`）或首次进入 `missing` 状态时立即推送告警，之后按冷却间隔（`alert_cooldown`，默认 `24h`）提醒，避免每次定时运行重复刷屏；当异常仓库重新同步成功时自动推送一条“已恢复”通知。
     * 周期健康度周报通过独立子命令 `git-backup report --weekly` 触发，彻底适配无状态定时调度架构。

---

## 4. CLI 子命令体系与交互规范

工具编译为单一可执行文件 `git-backup`。配置文件查找优先级为：`--config` 参数 > 环境变量 `GIT_BACKUP_CONFIG` > `./config.yaml` > `/etc/git-backup/config.yaml`。核心命令规范如下：

```bash
# 1. 执行一次完整白名单备份同步（供 systemd timer / cron 调用）
git-backup sync --config /etc/git-backup/config.yaml

# 2. 预检白名单配置（校验格式重复项、探测远端可达性/改名，列出待失管仓库，不写磁盘）
git-backup targets --check --config /etc/git-backup/config.yaml

# 3. 查看全局同步状态概览与异常仓库明细（支持 --root 脱离配置文件读取目录）
git-backup status
git-backup status --root /data/github-backups
git-backup status --failed          # 仅列出 failed 与 missing 仓库及原因
git-backup status --stale 168h      # 列出超过 7 天未完成同步校验的仓库
git-backup status --json            # 输出原始 JSON 供自动化脚本消费

# 4. 远端与本地引用树强一致性实时比对（ls-remote vs show-ref，自动跳过 missing/unmanaged）
git-backup verify                   # 全量比对所有可达白名单仓库引用哈希
git-backup verify --json            # 输出机器可读校验报告
git-backup verify muidea/clawStation --fsck   # 指定单库比对引用并执行 git fsck 完整性检查

# 5. 推送每周全局备份健康度摘要（读取本地账本，供独立周报定时器调用）
git-backup report --weekly --config /etc/git-backup/config.yaml
```

**`git-backup status` 终端输出示意**：

```text
=== Last Backup Summary (Run ID: 20261118-020001) ===
Started: 2026-11-18 02:00:01 UTC | Duration: 24.6s | Result: PARTIAL_FAILURE (Exit 2)
Total Managed: 42 | Cloned: 1 | Updated: 4 | Up-to-date: 35 | Failed: 1 | Missing(Retained): 1 | Unmanaged: 2

[!] ABNORMAL REPOSITORIES (2)
REPO                     STATE    FAILS  LAST SUCCESS          DETAIL / ERROR SUMMARY
muidea/legacy-monolith   failed   3      2026-11-17 14:00:12   RPC failed; curl 56 GnuTLS recv error (-110)
muidea/old-service       missing  1      2026-11-16 02:00:08   404 Not Found (local backup retained intact)

[*] MANAGED REPOSITORIES (Top Recent Updated)
REPO                     STATUS       HEAD      LAST SYNC             DISK SIZE  LFS
muidea/clawStation       updated      a1b2c3d   2026-11-18 02:00:19   412.5 MB   on
muidea/fedquery-engine   updated      9f8e7d6   2026-11-18 02:00:14   86.2 MB    off
muidea/billing-core      up_to_date   4c5b6a7   2026-11-18 02:00:03   128.0 MB   off
```

---

## 5. 本地存储目录与状态账本数据结构

### 5.1 目录树结构规范

```text
/data/github-backups/
├── .backup.lock                      # 全局单实例排他文件锁 (flock)
├── .tmp_clones/                      # 首次克隆临时暂存区 (成功后原子 mv 至 repos/，防半成品)
├── status_summary.json               # 最近一次备份批次的全局运行摘要
├── repo_ledger.json                  # 全量仓库同步状态与元数据账本
├── repos/                            # 活跃裸仓库存储根目录 (含 retain 的失管库与保留的 missing 库)
│   ├── muidea/
│   │   ├── clawStation.git/          # 镜像裸仓库 (含 objects, refs, HEAD, config, lfs)
│   │   ├── fedquery-engine.git/
│   │   └── old-service.git/          # 远端 404 时本地依然原地保留，仅账本标记 state=missing
│   └── torvalds/
│       └── linux.git/
└── .trash/                           # 软隔离归档区 (超过 retention_days 自动物理清理并清理账本)
    ├── unmanaged_20261110_103000/    # 当 unmanaged_policy=archive 时移出白名单的仓库
    └── conflict_20261112_080000/     # 同路径但 repo_id 冲突时被安全隔离的旧仓库
```

### 5.2 全局运行摘要（`status_summary.json`）

```json
{
  "schema_version": 2,
  "run_id": "20261118-020001",
  "started_at": "2026-11-18T02:00:01Z",
  "finished_at": "2026-11-18T02:00:25Z",
  "duration_ms": 24610,
  "exit_code": 2,
  "bulk_missing_alert_triggered": false,
  "counts": {
    "managed_total": 42,
    "cloned": 1,
    "updated": 4,
    "up_to_date": 35,
    "failed": 1,
    "missing": 1,
    "unmanaged": 2
  },
  "abnormal_repos": [
    {
      "repo": "muidea/legacy-monolith",
      "state": "failed",
      "consecutive_failures": 3,
      "last_error": "error: RPC failed; curl 56 GnuTLS recv error (-110): The TLS connection was non-properly terminated."
    },
    {
      "repo": "muidea/old-service",
      "state": "missing",
      "consecutive_failures": 1,
      "last_error": "GitHub API returned 404 Not Found; local backup retained at repos/muidea/old-service.git"
    }
  ]
}
```

### 5.3 仓库级状态账本（`repo_ledger.json`）

以不可变字符串化的 GitHub `repo_id` 作为主键，天然支持仓库重命名追踪与同路径冲突识别：

```json
{
  "schema_version": 2,
  "updated_at": "2026-11-18T02:00:25Z",
  "repositories": {
    "68192031": {
      "id": 68192031,
      "full_name": "muidea/clawStation",
      "local_path": "repos/muidea/clawStation.git",
      "clone_url": "https://github.com/muidea/clawStation.git",
      "state": "updated",
      "etag": "W/\"a8f9c2d10e4b\"",
      "remote_pushed_at": "2026-11-18T01:45:10Z",
      "last_git_sync_at": "2026-11-18T02:00:19Z",
      "last_checked_at": "2026-11-18T02:00:19Z",
      "last_alert_at": "",
      "head_commit": "a1b2c3d4e5f678901234567890abcdef12345678",
      "disk_size_bytes": 432537600,
      "lfs_enabled": true,
      "sync_count_since_gc": 8,
      "consecutive_failures": 0,
      "last_error": ""
    }
  }
}
```

---

## 6. 声明式配置文件规范（`config.yaml`）

```yaml
storage:
  root_dir: "/data/github-backups"
  trash_retention_days: 30
  gc_interval_syncs: 25
  min_free_disk_percent: 10     # 挂载点剩余空间低于 10% 时提前告警并中止同步
  min_free_disk_gb: 20          # 与百分比取“或”条件，适配大容量 NAS 防止绝对空间浪费

auth:
  # 方式一：环境变量注入 PAT（推荐个人或轻量团队使用）
  mode: "pat"                   # pat | github_app
  token_env: "GITHUB_BACKUP_TOKEN"
  # 方式二：GitHub App 认证（企业组织推荐，更高 API 速率限制与动态续期）
  # app_id: 123456
  # installation_id: 78901234
  # private_key_path: "/etc/git-backup/app-private-key.pem"

# 全局默认同步策略（可被白名单单库配置覆盖）
defaults:
  enable_lfs: "auto"            # true | false | auto（基于 HEAD:.gitattributes / lfs 目录探测）
  mirror_pr_refs: true          # 是否包含 refs/pull/*；超大活跃仓库可设为 false
  clone_timeout: "1h"
  update_timeout: "15m"
  unmanaged_policy: "retain"    # retain（原地保留并标记失管） | archive（移入 .trash）

# 显式仓库白名单（仅备份以下明确列出的 github.com 仓库，不进行全量自动发现）
targets:
  - repo: "muidea/clawStation"
    enable_lfs: true
    update_timeout: "30m"
  - repo: "muidea/fedquery-engine"
  - repo: "muidea/billing-core"
    skip_unchanged: false       # 核心库每次强制校验同步，不依赖 pushed_at 跳过
  - repo: "torvalds/linux"
    clone_timeout: "4h"
    update_timeout: "45m"
    mirror_pr_refs: false       # 禁用 PR 引用镜像，避免海量 refs 拖慢增量同步

sync:
  concurrency: 6
  skip_unchanged: true          # 基于远端 pushed_at 跳过无推送且健康的仓库
  force_full_sync_days: 7       # 即使 pushed_at 未变，每满 7 天强制执行一次 Git 引用校准
  max_retries: 3

network:
  proxy_url: ""                 # 示例: "http://127.0.0.1:7890" 或 "socks5://127.0.0.1:1080"

safety:
  max_missing_alert_ratio: 0.30 # 白名单内远端 404 比例超 30%（且至少 2 个）时触发高优异常告警

observability:
  log_format: "json"            # json | text
  log_level: "info"
  prometheus_textfile_path: "/var/lib/node_exporter/textfile_collector/git_backup.prom"
  alert:
    webhook_type: "feishu"      # feishu | wecom | dingtalk | slack | generic
    webhook_url_env: "BACKUP_ALERT_WEBHOOK"
    webhook_secret_env: ""      # 可选，飞书/钉钉开启签名校验时读取加签密钥的环境变量名
    failure_threshold: 2        # 单个仓库连续失败达到 2 次时触发告警（过滤偶发抖动）
    alert_cooldown: "24h"       # 同一持续异常的重复提醒冷却时间；恢复正常后立即发恢复通知
```

---

## 7. 边界场景与异常处理策略

| 异常/边界场景 | 潜在风险 | 系统应对策略 |
| :--- | :--- | :--- |
| **白名单仓库在远端不存在或失权** | 误删本地最后一根救命稻草，或被强行移走导致灾备断裂 | **本地只增不删**：原地保留 `repos/<owner>/<repo>.git` 不做任何破坏性操作，账本标记 `missing` 并发送告警（单轮大面积 404 触发高优告警） |
| **从白名单移除仓库导致失管** | 若按异常或删库处理会误发告警或误删历史备份 | **对账解耦**：识别为 `unmanaged` 而非 `missing`，默认 `retain` 原地保留并在 `status` 中提示；配合 `targets --check` 变更前预检 |
| **首次大仓克隆中途崩溃 / OOM** | 目标路径残留残缺 `.git` 目录，下轮误走 `remote update` 报错 | 先在 `.tmp_clones/` 下执行克隆，完全成功后原子 `os.Rename` 至 `repos/`；每次启动自动清理 `.tmp_clones/` 残留 |
| **远端删库后重建同名仓库** | `full_name` 相同但 `repo_id` 变更，本地目录冲突导致新库无法落位 | 对账发现目标路径已被另一 `repo_id` 占用时，自动先将旧库移入 `.trash/conflict_<ts>/` 隔离（保留 $30$ 天），再落位新库 |
| **空仓库（新建尚未提交任何代码）** | `clone --mirror` 后无 `HEAD` 指向分支，`rev-parse HEAD` 返回非零 | 克隆/拉取后以 `git -C <dir> show-ref` 为空作为权威判定，账本记录 `head_commit: "EMPTY"`，跳过 LFS 阶段，标记成功 |
| **远端分支与目录层级同名冲突（D/F 冲突）** | 远端删除分支 `feat` 后新建 `feat/login`，引用拉取可能冲突报错 | 捕获 D/F 引用冲突特征后，自动先执行 `git -C <dir> remote prune origin` 清理旧引用，再重试 `remote update --prune` |
| **新建轻量 Tag 未触发 `pushed_at` 变化** | 纯依赖 `pushed_at` 可能导致新打的 Tag 延迟同步 | 通过 `force_full_sync_days: 7` 周期强制同步兜底；对时效极敏感的核心库可在白名单项中单独配置 `skip_unchanged: false` |
| **上次备份进程因大仓克隆尚未结束** | 定时周期重叠引发同一裸仓库并发写损坏 | 进程启动首步对 `.backup.lock` 申请非阻塞 `flock`，获取失败输出提示日志并以状态码 `0` 退出 |
| **本地磁盘空间耗尽** | Packfile 写入中断，残留 `tmp_pack_*` 锁死磁盘 | 启动前校验剩余空间比例与绝对 GB 阈值；单任务失败后自动扫描清理 `objects/pack/tmp_*` |
| **灾备恢复与代码检出** | 运维人员直接在裸仓库目录寻找源码文件 | 明确规范：恢复工作区执行 `git clone /data/github-backups/repos/org/repo.git ./worktree`；整库回推新平台执行 `git -C <bare_dir> push --mirror <new_url>` |

---

## 8. 部署与调度落地建议

推荐将工具部署在具备独立数据盘的 Linux 服务器或 NAS 节点上，采用 `systemd` 管理生命周期与资源边界：

1. **主同步服务单元（`/etc/systemd/system/git-backup.service`）**：
   * 配置 `Type=oneshot`，通过 `EnvironmentFile=/etc/git-backup/secret.env`（权限 `0600`）隔离加载 `GITHUB_BACKUP_TOKEN` 与 `BACKUP_ALERT_WEBHOOK`。
   * 配置 `SuccessExitStatus=0 2`，避免部分仓库异常（退出码 `2`，已由工具内部按边缘防抖策略发送 Webhook）重复触发 `OnFailure=git-backup-emergency-notify.service`；将 `OnFailure` 严格保留用于全局致命异常（退出码 `1`）或进程崩溃/OOM 兜底。
   * 设置 `IOSchedulingClass=best-effort` 与 `Nice=10`，确保周期性 `git maintenance` 压缩打包不影响宿主机在线服务。
2. **主同步定时单元（`/etc/systemd/system/git-backup.timer`）**：
   * 配置 `OnCalendar=0/4:00:00`（每 $4$ 小时执行一次增量备份）及 `RandomizedDelaySec=5m`（错峰请求 GitHub API）。
   * 开启 `Persistent=true`，保障宿主机断电维护重启后自动补齐错过的备份任务。
3. **周报定时单元（可选，`git-backup-report.timer`）**：
   * 配置 `OnCalendar=Mon *-*-* 09:00:00`，拉起 `git-backup report --weekly --config /etc/git-backup/config.yaml` 推送全局健康度周报。
