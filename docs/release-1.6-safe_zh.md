# 1.6.0 定制安全版发布核验

本说明是发布准备流程，不表示 `1.6.0-tg.35-safe` 已发布或生产切换已完成。发布与部署是独立步骤，生产升级仍须遵守 [离线迁移说明](upgrade-1.6-safe_zh.md)。

## 已核对的工作流行为

| 工作流 | 触发与实际产物 | 注意事项 |
| --- | --- | --- |
| `build.yml` | PR / 手工构建，产生前端及 8 个平台的 Actions 构建产物 | 内嵌版本是提交 SHA；不是正式版本 Release，也不发布镜像。 |
| `release.yml` | GitHub Release 发布事件：上传 8 个二进制，构建 Linux amd64/arm64 多架构镜像 | 镜像标签来自 Release tag。仅 GitHub 将该 Release 标为 Latest 时，额外更新 Docker `latest`。二进制上传没有 `--clobber`。 |
| `release-docker.yml` | 手工指定版本，只构建并推送双架构镜像 | 不创建 Release，不上传其二进制；同名镜像标签可能覆盖已发布内容。 |
| `docker-publish.yml` | 手工运行，按 ref 名及短 SHA 推送镜像 | 内嵌版本为 ref 名，不作为本次正式发布入口。 |
| `rebuild-release.yml` | 手工重建现有 Release，二进制使用 `--clobber`，覆盖指定镜像 tag | 当前矩阵只有 7 个平台，未含 Linux loong64，且部分 action 使用上游 `main`；不用于本次常规首次发布。 |
| `snapshot.yml` | `main` 推送 / 手工执行，生成分钟级 Snapshot 预发布和 `snapshot` 镜像 | 会清理旧 Snapshot，重跑同名 Snapshot 可能删除并重建标签；不能用作不可变的正式版本。 |

正式 `release.yml` 的 8 个二进制名称为：

```text
komari-linux-amd64
komari-linux-arm64
komari-linux-386
komari-linux-riscv64
komari-linux-loong64
komari-windows-amd64.exe
komari-windows-arm64.exe
komari-windows-386.exe
```

正式镜像名称为 `ghcr.io/rookib/komari:1.6.0-tg.35-safe`，镜像平台只有 Linux amd64、arm64。`komari-frontend` 是 Actions 内部构建产物；前端被嵌入服务器二进制，不会由此工作流单独成为公开主题下载包。

版本镜像由 `docker/metadata-action` 生成规范化名称。`release.yml` 的发布 job 和 `rebuild-release.yml` 的镜像 job 还会先把 `IMAGE_NAME` 转为小写，确保直接拼接的版本标签与 `latest` 引用均为 `ghcr.io/rookib/komari`；GitHub API/`gh --repo` 参数不变。发布失败不能用“版本镜像已上传”掩盖后续标签任务失败。

当前正式工作流不会自动生成 `SHA256SUMS`，也不会构建或上传 `migrate-metrics` 和部署脚本。不能把“8 个二进制上传完成”等同于升级工具已交付。

## 固定来源，避免名称碰撞

1. 对本次最终后端提交完成测试，记录完整 SHA；确认其中包含最后的 SQLite 统计准备与阶段日志修复。不能仅引用之前已通过的旧提交检查。
2. 前端构建 action 当前固定在 `Rookib/komari-web` 的 `42b6164f86c6e7f457da4dcf3d4cc3272757519b`。本次 tag `1.6.0-tg.35-safe` 会使用该值。特别注意：名称匹配 `*-fix数字` 的 tag 会被 `release.yml` 改为使用去掉 `-fix数字` 后的前端 ref，不能随意以此命名补发版本。
3. 发布前分别检查 Git tag、GitHub Release 和 GHCR 版本 tag 是否已存在，只有明确不存在才创建；鉴权、网络或权限错误不能当作“不存在”。已有名称若属于不同 SHA/digest，分配新的定制版本号并同步发布说明、内嵌版本和验收配置，不强推 tag、不删除重建正式 Release、不覆盖镜像。
4. 同一版本只使用 `release.yml` 一个发布入口，不同时运行手工 Docker 发布、重建 Release 或 Snapshot 来写同名产物。正式稳定版可按发布策略标为 Latest，但应明确这会同时移动 Docker `latest`；实际部署仍固定版本和 digest。
5. 若部分任务失败，先检查已有资产、构建 SHA 和镜像状态，只重试确认失败且没有同名成功产物的步骤。不要用 `--clobber` 掩盖不确定结果，也不要盲目重跑所有已成功上传的任务。

## 发布后、部署前的独立校验

- 核对该次 `release.yml` 的实际 commit、结论和全部矩阵任务，不以 PR 构建成功替代正式 Release 构建成功。
- 下载该 Release 的 8 个二进制，核对名称、数量、大小与平台格式，并生成/复核 `SHA256SUMS`。如需补充升级工具，使用单独名称，例如 `komari-safe-upgrade-tools-1.6.0-tg.35-safe-linux-amd64.tar.gz`，包含同一后端 SHA 构建的迁移 CLI、部署脚本、其相对目录依赖和来源清单；不能覆盖已有二进制名称。
- 确认 SHA 清单对应最终下载字节，而非旧的本地产物。校验值可发现传输和版本混用问题，不代替对发布源的信任。
- 检查 GHCR manifest 包含 Linux amd64/arm64，记录 manifest digest 和所用平台镜像 digest。核对镜像内版本及构建 hash，确保与该 Release 的 tag/commit 一致。
- 在无网络、独立数据副本上运行同一版本迁移工具和审计。迁移 CLI 以 `status=complete, phase=verified` 且退出 0 为完成条件；随后还需审计、实际页面及上报验证。
- 保留“发布资产通过”“演练通过”“生产切换通过”三个独立结果。只有正式切换、数据核验、真实节点上报和前端验收都完成，交付报告才能改为已上线。
