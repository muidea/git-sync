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

### 1. 一键初始化配置并校验权限

支持交互式向导或单行参数初始化（自动生成 `config.yaml`、`0600` 权限的 `secret.env`，并自动运行 `targets --check` 验证 Token 权限；`--repos` 支持直接传入 `owner/repo` 或 `git@github.com:...` / `https://github.com/...` 地址）：

```bash
# 交互式初始化
git-backup init

# 或非交互式一步完成配置、验证并托管 systemd 定时任务（非 root 可加 --user-service）
sudo git-backup init \
  --root /data/github-backups \
  --repos "muidea/magicCommon,muidea/magicRunner" \
  --token "github_pat_xxx" \
  --install-service
```

> **GitHub Fine-grained PAT 最小权限要求**：在 GitHub Token 设置中为目标仓库授予 **`Metadata: Read-only`** 与 **`Contents: Read-only`**。若仅勾选 `Metadata` 而不勾选 `Contents`，预检可通但 `git clone` 会返回 `403`。

如需手动编写配置文件，可参考 [configs/config.example.yaml](./configs/config.example.yaml)。

### 2. 手动执行同步与校验

```bash
# 执行同步（自动查找配置文件，也可通过 --config 指定）
git-backup sync

# 查看状态表格（也可通过 --root 脱离配置文件读取）
git-backup status
git-backup status --failed
git-backup status --stale 168h --json

# 实时比对远端 ls-remote 与本地 show-ref，并检查本地 Git 对象完整性
git-backup verify --fsck
```

配置文件查找优先级：`--config` 参数 > 环境变量 `GIT_BACKUP_CONFIG` > `./config.yaml` > `~/.config/git-backup/config.yaml` > `/etc/git-backup/config.yaml`。

---

## CLI 命令与退出码说明

| 命令 | 说明 |
| :--- | :--- |
| `git-backup init [flags]` | 交互/非交互初始化配置、密钥文件并校验远端可达性 |
| `git-backup service <install\|uninstall\|status>` | 一键安装/卸载/查看 `systemd` 定时任务（支持 `--user` 无 root 运行） |
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

## 生产环境部署

### 方式一：`systemd` 托管（内置一键安装）

```bash
# 系统级定时任务（默认每 4 小时同步一次；加 --with-report 同时开启周一 09:00 周报）
sudo git-backup service install --with-report

# 普通用户级定时任务（无需 root）
git-backup service install --user

# 查看定时器状态与日志
git-backup service status
journalctl -u git-backup.service -n 50 --no-pager

# 预览生成的 unit 文件（不落盘）或卸载
git-backup service install --dry-run
sudo git-backup service uninstall
```

### 方式二：Docker / Docker Compose 部署（推荐 NAS / 老版本系统）

容器镜像（支持 `linux/amd64`、`linux/arm64`）内置最新 `git` 与 `git-lfs`，无需在宿主机安装或升级依赖。

无需克隆代码仓库，新建部署目录并创建 `docker-compose.yml` 与 `config.yaml` 即可启动（完整带注释模板见 [deploy/docker-compose.example.yml](./deploy/docker-compose.example.yml)）：

```yaml
# docker-compose.yml
services:
  git-backup:
    image: ghcr.io/muidea/git-sync:latest
    container_name: git-backup
    restart: unless-stopped
    environment:
      TZ: "Asia/Shanghai"
      CRON_SCHEDULE: "0 */4 * * *"
      RUN_ON_START: "true"
      # WEEKLY_REPORT_CRON: "0 9 * * 1"
      GITHUB_BACKUP_TOKEN: "${GITHUB_BACKUP_TOKEN:-}"
      BACKUP_ALERT_WEBHOOK: "${BACKUP_ALERT_WEBHOOK:-}"
      BACKUP_ALERT_SECRET: "${BACKUP_ALERT_SECRET:-}"
    volumes:
      - ./config.yaml:/etc/git-backup/config.yaml:ro
      - ./backups:/data/github-backups
      # 如使用 GitHub App，额外挂载私钥：
      # - ./app-private-key.pem:/etc/git-backup/app-private-key.pem:ro
```

配套最小 `config.yaml` 示例（`storage.root_dir` 保持容器内路径 `/data/github-backups`）：

```yaml
storage:
  root_dir: "/data/github-backups"
  trash_retention_days: 30
  gc_interval_syncs: 25
  min_free_disk_percent: 10
  min_free_disk_gb: 20

auth:
  mode: "pat"
  token_env: "GITHUB_BACKUP_TOKEN"

defaults:
  enable_lfs: "auto"
  mirror_pr_refs: true
  clone_timeout: "1h"
  update_timeout: "15m"
  unmanaged_policy: "retain"

targets:
  - repo: "muidea/magicCommon"
  - repo: "muidea/magicRunner"

sync:
  concurrency: 6
  skip_unchanged: true
  force_full_sync_days: 7
  max_retries: 3
```

启动与日常运维命令：

```bash
# 1. 创建同目录 .env（或直接 export GITHUB_BACKUP_TOKEN）
cat > .env <<'EOF'
GITHUB_BACKUP_TOKEN=github_pat_xxx
EOF
chmod 600 .env

# 2. 启动后台定时备份（启动时立即执行首次同步，之后按 CRON_SCHEDULE 调度）
docker compose up -d

# 3. 查看日志、状态或手动触发校验
docker compose logs -f
docker compose exec git-backup git-backup status
docker compose exec git-backup git-backup verify --fsck
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
