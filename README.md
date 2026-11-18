# git-sync (`git-backup`)

基于显式白名单驱动的 GitHub 仓库定期裸镜像备份工具（Go 单二进制无状态 CLI）。完整保留远端分支、标签、Notes 及可选 Pull Request 引用（`refs/pull/*`）与 Git LFS 大文件对象，具备只增不删防连带损毁、原子状态账本、实时一致性校验及监控告警能力。

详细系统设计参见：[系统设计文档 (v2.0)](./docs/github_v2.md)。

---

## 核心特性

* **纯显式白名单管控**：仅备份 `config.yaml` 中 `targets` 显式指定的 `owner/repo`，支持单库覆盖 `enable_lfs`、`mirror_pr_refs`、`skip_unchanged` 及超时参数。
* **轻量 API 路由**：$\le 20$ 个仓库并发调用单库 REST（带 `If-None-Match` `ETag` 省配额）；$> 20$ 个自动分批走 GraphQL 节点查询。
* **只增不删保护**：严格区分“移出白名单（`unmanaged`）”与“白名单内远端 404/失权（`missing`）”。远端返回 404 时**原地保留本地已有备份目录**，绝不自动删移，同时触发告警。
* **重命名迁移 & 存量接管**：依托不可变 `repo_id` 识别仓库改名并原地原子迁移；首次启用可自动接管已有合法裸仓库目录（Adopt），避免大仓重拉。
* **零明文密钥残留**：凭证通过 `GIT_CONFIG_COUNT` 环境变量注入 `git` / `git-lfs` 子进程，绝不写入 `.git/config` 或命令行参数；支持 GitHub App Token 过期前自动续期。
* **全链路可观测**：原子写入 `repo_ledger.json` 与 `status_summary.json`，内置 `status`、`verify --fsck` 深度校验、Prometheus Textfile 指标导出及飞书/钉钉/企微/Slack/Generic Webhook 告警与周报。

---

## 环境要求

* **操作系统**：Linux（推荐配合 `systemd` 运行）或 macOS
* **Git**：`git >= 2.31`（用于支持 `GIT_CONFIG_COUNT` 环境变量注入认证头）
* **Git LFS**（可选）：若启用 LFS 备份（`enable_lfs: true` 或仓库触发 `auto` 探测），宿主机需安装 `git-lfs`
* **Go**（仅源码编译需要）：`go >= 1.24`（开发测试使用 Go 1.26）

---

## 安装指南

### 方式一：一键脚本安装（默认安装最新版本，推荐）

```bash
curl -fsSL https://raw.githubusercontent.com/muidea/git-sync/master/scripts/install.sh | bash
```

如需指定历史版本或自定义安装目录：

```bash
VERSION="0.0.1" INSTALL_DIR="/usr/local/bin" \
  bash -c "$(curl -fsSL https://raw.githubusercontent.com/muidea/git-sync/master/scripts/install.sh)"
```

### 方式二：手动下载预编译包（默认拉取最新 Release）

从 [GitHub Releases](https://github.com/muidea/git-sync/releases) 获取最新版本号并安装（按需替换 `linux_amd64` 为 `linux_arm64` / `darwin_amd64` / `darwin_arm64`）：

```bash
TAG=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/muidea/git-sync/releases/latest | awk -F/ '{print $NF}')
VERSION="${TAG#v}"
curl -fsSLO "https://github.com/muidea/git-sync/releases/download/${TAG}/git-backup_${VERSION}_linux_amd64.tar.gz"
tar -xzf "git-backup_${VERSION}_linux_amd64.tar.gz"
sudo install -m 0755 "git-backup_${VERSION}_linux_amd64/git-backup" /usr/local/bin/git-backup
git-backup version
```

### 方式三：从源码编译

```bash
git clone git@github.com:muidea/git-sync.git
cd git-sync
go build -trimpath -ldflags="-s -w" -o ./bin/git-backup ./cmd/git-backup
sudo install -m 0755 ./bin/git-backup /usr/local/bin/git-backup
```

---

## 快速开始

### 1. 准备配置文件

参考 [configs/config.example.yaml](./configs/config.example.yaml) 创建 `/etc/git-backup/config.yaml`：

```yaml
storage:
  root_dir: "/data/github-backups"
  trash_retention_days: 30
  gc_interval_syncs: 25
  min_free_disk_percent: 10
  min_free_disk_gb: 20

auth:
  mode: "pat"                   # pat | github_app | none（仅公开库可用 none）
  token_env: "GITHUB_BACKUP_TOKEN"

defaults:
  enable_lfs: "auto"            # true | false | auto
  mirror_pr_refs: true
  clone_timeout: "1h"
  update_timeout: "15m"
  unmanaged_policy: "retain"    # retain | archive

targets:
  - repo: "muidea/magicCommon"
  - repo: "muidea/magicRunner"
    update_timeout: "30m"

sync:
  concurrency: 6
  skip_unchanged: true
  force_full_sync_days: 7
  max_retries: 3
```

> **GitHub Fine-grained PAT 最小权限要求**：在 GitHub Token 设置中为目标仓库授予 **`Metadata: Read-only`** 与 **`Contents: Read-only`**。若仅勾选 `Metadata` 而不勾选 `Contents`，预检可通但 `git clone` 会返回 `403`。

### 2. 配置环境变量并预检

```bash
export GITHUB_BACKUP_TOKEN="github_pat_xxx"

# 仅校验配置与远端可达性，不写磁盘
git-backup targets --check --config /etc/git-backup/config.yaml
```

### 3. 执行同步与校验

```bash
# 执行同步
git-backup sync --config /etc/git-backup/config.yaml

# 查看状态表格（也可通过 --root 脱离配置文件读取）
git-backup status --config /etc/git-backup/config.yaml
git-backup status --failed
git-backup status --stale 168h --json

# 实时比对远端 ls-remote 与本地 show-ref，并检查本地 Git 对象完整性
git-backup verify --fsck --config /etc/git-backup/config.yaml
```

配置文件查找优先级：`--config` 参数 > 环境变量 `GIT_BACKUP_CONFIG` > `./config.yaml` > `/etc/git-backup/config.yaml`。

---

## CLI 命令与退出码说明

| 命令 | 说明 |
| :--- | :--- |
| `git-backup sync [--config <path>]` | 执行一次完整白名单对账与镜像同步 |
| `git-backup targets --check [--config <path>]` | 预检配置语法、远端可达性/改名与待失管清单 |
| `git-backup status [--root <dir>] [--failed] [--stale 168h] [--json]` | 读取本地账本展示同步健康状态 |
| `git-backup verify [owner/repo] [--fsck] [--json]` | 实时比对远端与本地全引用哈希一致性 |
| `git-backup report --weekly [--config <path>]` | 读取本地账本向 Webhook 推送每周健康度周报 |
| `git-backup version` | 输出版本信息 |

**进程退出码**：
* `0`：全部成功（含跳过或全局排他锁被占用时安全退出）
* `1`：全局致命失败（配置错误、依赖版本不足、401/403 认证异常、API 配额耗尽、磁盘空间不足）
* `2`：部分仓库异常（主流程完成，但存在 `failed` 或远端 `missing` 仓库）

---

## 生产环境 `systemd` 部署

仓库 [deploy/](./deploy) 目录已提供开箱即用的 `systemd` 单元模板：

```bash
# 1. 创建隔离密钥文件（仅 root 可读）
sudo mkdir -p /etc/git-backup /data/github-backups
sudo tee /etc/git-backup/secret.env >/dev/null <<'EOF'
GITHUB_BACKUP_TOKEN=github_pat_xxx
BACKUP_ALERT_WEBHOOK=https://open.feishu.cn/open-apis/bot/v2/hook/xxx
EOF
sudo chmod 0600 /etc/git-backup/secret.env

# 2. 安装并启用服务与定时器
sudo cp deploy/git-backup.service /etc/systemd/system/
sudo cp deploy/git-backup.timer /etc/systemd/system/
sudo cp deploy/git-backup-report.timer /etc/systemd/system/   # 可选：每周一 09:00 周报
sudo systemctl daemon-reload
sudo systemctl enable --now git-backup.timer

# 3. 查看定时任务与日志
systemctl list-timers git-backup*
journalctl -u git-backup.service -n 50 --no-pager
```

---

## 灾备恢复指南

本地存储的是标准 Git 裸镜像仓库，不含工作区源码文件：

* **检出源码工作区**：
  ```bash
  git clone /data/github-backups/repos/muidea/magicRunner.git ./magicRunner-worktree
  ```
* **整库镜像回推至新托管平台**：
  ```bash
  git -C /data/github-backups/repos/muidea/magicRunner.git push --mirror <new_remote_url>
  ```

---

## 本地开发与测试

```bash
go vet ./...
go test -count=1 -v -race ./...
```

## License

[MIT](./LICENSE)
