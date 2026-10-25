# Komari 1.6.0 定制安全版升级

基于官方后端 `1.6.0` 和前端 `1.6.0`，合并本仓库 `agent/nosla-tg-calibration` 的完整定制功能。保留安全控制台、SSH 登录与固定封禁/解封队列、邮件及 Telegram 多渠道、流量报告、重置提醒、vnStat、官方流量缓存和 DMIT/NoSLA 校准。

## 安全边界

- 安装入口只提供 Linux amd64/arm64 的 `Rookib/tcpping` 安全版，不回退官方可远程执行的 agent。
- `admin:getAgentStartupConfig` / `admin:switchAgentVersion` 和对应 agent 发送、队列入口禁用；不会读取探针启动凭据或替换线上探针。
- Agent 上报必须使用认证后的 Agent 身份；不能用管理员身份加节点 token 冒充上报。基础信息只能修改遥测字段，不能更改节点令牌、计费、可见性和流量锚点。
- 固定 nftables ban/unban 同步保留，拒绝任意命令、非单 IP、非法动作和超长时限。
- Telegram 请求要求 HTTPS、校验证书、拒绝重定向并限制超时；邮件认证必须在 TLS/STARTTLS 内完成。实际通知测试请使用本地捕获服务。

现有安全版 `v1.2.13-safe.13` 使用 v2 上报，不因本次面板升级自动替换。官方 1.6 已移除 v1 上报、剪贴板、负载告警及全局 xterm 设置；升级前检查仍在使用的节点协议和功能。

## 离线迁移与回滚

先在独立的数据副本中演练。保留完整 `data/`，包括主库、指标库、主题、密钥和流量校准状态。禁止把演练连接指向真实通知、探针入口或外部指标数据库。

构建 `go build -o migrate-metrics ./tools/migrate-metrics`。停止会写入这份数据的服务后，显式指定独立备份：

```sh
migrate-metrics --data-dir /app/data --backup-dir /backup --legacy-timezone UTC --confirm-offline
```

`--legacy-timezone` 应与旧容器的实际 TZ 相同；旧容器未设 TZ 时使用 UTC。迁移和最终服务必须使用相同的数据目录绝对路径。容器演练使用 `--network none`，备份挂载只读。工具不会丢弃历史，遇到非空的旧 SQL 监控表会停止，交由单独的历史迁移流程处理。

`tools/deploy-safe-upgrade/audit_data.py` 可比对节点/认证信息的摘要、计费及锚点、自定义表数量、既有通知配置和最近流量累计值。不要将数据库或通知凭据提交到仓库。

生产切换前停服务并再做一致性备份；保存旧容器和镜像。启动新版本后验证节点重连、真实上报、通知配置与前端版本。失败时必须恢复整份升级前数据，再启动旧容器，不能让旧程序读取已迁移数据库。

配套 `tools/dmit-wan-calibrator/dmit_wan_calibrator.py` 同时支持旧纳秒表和 1.6 毫秒规范化表；升级时同步脚本但保留已有配置、密钥、锚点与状态文件。旧 Nginx 静态资源覆盖和浏览器缓存版本应随新前端一并校验。
