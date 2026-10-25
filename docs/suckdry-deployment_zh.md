# Komari 自用安全版部署教程

本文对应 `Rookib/komari` 的 `1.6.0-tg.35-safe` 定制实现，配合 `Rookib/tcpping` 安全 Agent。**全新安装与旧版升级使用不同流程**：已有数据库的面板必须先备份、在副本演练并完成离线迁移，不能直接替换镜像后启动。

本文中的面板版本和镜像名是本次升级目标，不代表镜像已经发布或生产部署已经完成。部署前应确认对应发布产物、校验值和实际镜像版本；不使用浮动的 `latest` 面板标签。

## 1. 版本与兼容范围

| 组件 | 本次版本 | 说明 |
| --- | --- | --- |
| 面板 | `1.6.0-tg.35-safe` | 基于官方前后端 1.6.0，保留 TCP-safe、安全控制台、SSH、流量和校准功能 |
| 镜像命名 | `ghcr.io/rookib/komari:1.6.0-tg.35-safe` | 目标标签；先确认产物可用，或使用从对应源码构建并验证的本地镜像 |
| 新装安全 Agent | `v1.2.13-safe.14` | 已发布 Linux amd64 / arm64 及 `SHA256SUMS`，修复特殊 IPv6 地址处理 |
| 现有安全 Agent | `v1.2.13-safe.13` | 通过 v2 协议上报，继续兼容本次面板；不自动升级现有节点 |
| Agent 安装平台 | Linux amd64 / arm64 | 安装入口固定使用 `Rookib/tcpping`，不回退官方 Agent 或其他平台产物 |

本次面板升级不会自动替换各台 VPS 上的 Agent。已经运行 `safe.13` 的节点无需为了面板升级而重新安装；更旧节点应先核对版本和 v2 上报情况，再单独安排升级。

官方 1.6 已移除 v1 上报、剪贴板、负载告警和全局 xterm 设置。本定制版保留已有负载规则数据供核验，但不恢复被官方移除的负载告警界面和执行能力。

## 2. 从旧版升级：先阅读迁移说明

已有 `komari.db`、`metrics.db` 或任何面板数据时，使用 [Komari 1.6.0 定制安全版升级](upgrade-1.6-safe_zh.md)，不要执行下一节的新装命令。

升级顺序：

1. 保存旧镜像、容器参数和完整数据目录，包括主库、指标库、主题、通知配置、密钥、流量锚点与校准状态。
2. 在独立副本中进行无网络的离线迁移演练。使用与旧服务一致的时区；旧容器未设置 `TZ` 时为 UTC。迁移容器与最终服务的数据挂载绝对路径必须一致，例如 `/app/data`。
3. 校验迁移前后的节点、认证和计费信息、通知规则、流量计数及历史记录。校准脚本也须更新为支持 1.6 数据结构的版本，保留原配置、密钥和状态。
4. 正式切换时停止所有相关写入者，包括面板及校准定时任务，再做最终一致性备份并迁移。演练期间生产新增的数据不能省略，不能直接用早先的演练副本替换生产数据。
5. 启动新版本，检查节点重连和真实数据更新，再恢复定时任务。若回滚，恢复整份升级前数据后启动旧镜像；不能让旧程序读取已迁移的数据库。

不使用旧教程的 `docker pull → docker rm → docker run` 直接升级方式。不要提前删除旧容器、旧镜像或备份，也不要在浏览器选择丢弃历史数据来跳过迁移。

## 3. 全新安装面板

本节只适用于空数据目录。准备 Docker、HTTPS 域名和支持 WebSocket 的反向代理。Agent 连接入口可以单独使用 `https://agent.example.com`，但必须转发到同一面板服务。

先取得并核验目标镜像，或从对应源码构建本地镜像。下面命令要求镜像已经存在于本机，并拒绝使用非空数据目录；不会自动拉取一个尚未确认发布的标签：

```sh
(
  set -eu
  KOMARI_IMAGE='ghcr.io/rookib/komari:1.6.0-tg.35-safe'
  docker image inspect "$KOMARI_IMAGE" >/dev/null
  sudo mkdir -p /opt/komari/data
  if [ -n "$(sudo find /opt/komari/data -mindepth 1 -maxdepth 1 -print -quit)" ]; then
    echo '数据目录非空，请改用离线迁移升级流程。' >&2
    exit 1
  fi
  docker run -d \
    --name komari \
    --restart unless-stopped \
    -p 127.0.0.1:25774:25774 \
    -v /opt/komari/data:/app/data \
    "$KOMARI_IMAGE"
)
```

示例只监听服务器本机 `127.0.0.1:25774`。通过 Nginx、Caddy 或 Cloudflare Tunnel 提供 HTTPS 后，用配置的域名访问；该绑定方式不能直接通过 `http://服务器IP:25774` 访问。

首次启动在服务器本机查看初始化日志，取得初始登录信息并修改密码：

```sh
docker logs --tail 80 komari
```

初始化日志可能包含登录凭据，不要将原文公开或提交到仓库。

## 4. 通知渠道与规则

当前入口是 **通知 → 通知渠道**，路径 `/admin/notification/channels`。原来的 `/admin/settings/message-sender` 已移除。

本定制版为 `telegram`、`email` 和 Webhook 注册了兼容渠道。已有 Telegram / Email 配置会继续由兼容层读取；不要因官方 1.6 的通用插件提示而删除已有凭据。其他渠道以当前已注册的插件为准。

配置步骤：

1. 打开通知总开关，在“启用的通知渠道”中勾选需要的渠道并保存，可同时选择 Telegram 和 Email。
2. 在“配置通知渠道”中逐个选择渠道、填写并保存对应字段。选择要编辑的渠道不等于启用该渠道。
3. 使用页面的测试发送按钮验证当前启用的渠道；测试会实际发送消息。
4. 分别设置各节点的离线、流量报告和 SSH 登录规则。

| 功能 | 当前页面 |
| --- | --- |
| 渠道、模板和测试发送 | `/admin/notification/channels` |
| 通用规则、SSH 爆破告警静默模式 | `/admin/notification/general` |
| 节点离线通知 | `/admin/notification/offline` |
| 日报、周报和月报 | `/admin/notification/traffic-report` |
| SSH 登录通知、来源白名单和事件 | `/admin/notification/ssh-login` |
| 安全控制台 | `/security/dashboard`，需管理员登录 |

### Telegram

| 字段 | 说明 |
| --- | --- |
| `bot_token` | BotFather 创建的机器人 Token |
| `chat_id` | 私聊或群组的数字 ID |
| `endpoint` | 默认 `https://api.telegram.org/bot`；保持 HTTPS 和证书校验 |
| `message_thread_id` | 可选，群组话题 ID |
| `command_menu_enabled` | 开启面板端 Telegram 查询菜单 |
| `command_allowed_users` | 允许操作的 Telegram 数字用户 ID，英文逗号分隔；群组必须配置 |
| `command_timezone` | 今日、昨日查询的时区，例如 `Asia/Shanghai` |

同一 Bot Token 只由一个程序轮询，避免其他程序同时使用 `getUpdates` 或 webhook。Bot 运行在面板端，不把 Token 下发给 Agent。

### Email

填写 SMTP 主机、端口、用户名、密码或授权码、发件人和收件人。端口 465 配合 `use_ssl=true` 使用直接 TLS；其他连接必须支持 STARTTLS 后才能认证。不能通过关闭证书校验或明文认证来解决连接错误。

## 5. 新节点安装安全 Agent

优先在面板添加节点后，复制该节点的“一键部署指令”。当前入口只生成 Linux 安全版安装命令，并正确引用端点、Token 和自定义参数。

手动示例仅使用占位值，执行前替换为实际 Agent 入口和该节点 Token：

```sh
curl -fsSL 'https://raw.githubusercontent.com/Rookib/tcpping/main/install.sh' | sudo bash -s -- \
  -e 'https://agent.example.com' \
  -t 'YOUR_NODE_TOKEN' \
  --disable-web-ssh \
  --disable-auto-update \
  --ssh-auth-guard \
  --ssh-auth-silent-mode \
  --ssh-auth-aggregate-by-ip \
  --ssh-auth-threshold '5' \
  --ssh-auth-window '60' \
  --ssh-auth-cooldown '1800'
```

安装脚本从 `Rookib/tcpping` 安全版 Release 下载对应架构文件，并验证 `SHA256SUMS`。脚本使用该仓库的最新安全版发布，当前新装版本为 `v1.2.13-safe.14`；执行前仍应确认仓库的实际发布版本，不要把仓库覆盖成官方 Agent。

如套餐每月 15 日按中国时区重置，可在同一条安装命令末尾追加：

```sh
--month-rotate '15' --timezone 'Asia/Shanghai'
```

这两项是安装命令参数，不是独立命令。请使用真实套餐的日期和时区；未指定 `--timezone` 时使用 VPS 的时区。GitHub 账号更名不要求更改现有面板或 Agent 的业务域名。

## 6. 已有 Agent 的升级与检查

`v1.2.13-safe.13` 可继续运行。面板侧的切换 Agent 版本和读取启动配置入口已禁用；升级 Agent 使用服务器本机的安全版脚本，不使用面板远程替换能力。

`safe.14` 已发布，但不会因此自动升级全部节点。只有确实需要升级且使用默认 `/opt/komari/agent`、`komari-agent.service` 的机器，才使用下面的升级入口：

```sh
curl -fsSL 'https://raw.githubusercontent.com/Rookib/tcpping/main/upgrade.sh' | sudo bash
```

脚本校验下载文件、备份旧二进制，保留现有端点、Token、重置日和 TCP 白名单，并重启服务。它还写入时区及安全配置：开启 SSH 检测、静默模式和固定安全动作同步，默认关闭自动封禁。已有自定义启动参数可能覆盖环境配置，应核对最终生效行为；自定义安装目录或服务名不能直接套用此脚本。

不要通过读取并拼接整个 `ExecStart` 来修改参数，也不要把含 Token 的服务配置粘贴到公开日志。检查运行状态和必要的版本日志即可：

```sh
systemctl is-active komari-agent
journalctl -u komari-agent -n 80 --no-pager | grep -Ei 'Komari Agent|SSH auth guard|WebSocket connected'
```

验收应同时看到预期安全版版本、v2 连接和面板中的持续数据更新，不能只以服务显示 active 判断成功。

## 7. SSH 安全功能的默认行为

默认检测规则为：60 秒内同一来源 IP 失败 5 次触发记录，按 IP 聚合不同目标用户，同一来源冷却 1800 秒；默认不限于 root。

面板的 `ssh_auth_guard_silent_mode` 默认为开启：仍接收并记录爆破事件，但不向外推送这类告警。需要推送时，在“通知 → 通用设置”关闭静默模式，并核对通知总开关、节点 SSH 通知开关及来源白名单。成功登录通知与爆破告警是不同事件。

本机自动封禁默认关闭。确需启用时，先配置本机白名单并使用 `--ssh-auth-ban-dry-run` 验证；实际封禁仅支持固定 nftables 动作，不回退其他封禁工具。

安全控制台支持经过认证的固定 ban/unban 队列，由安全 Agent 轮询处理；这与自动封禁开关分开。队列只接受单个合法 IP 和受限时长，不接受任意 shell 命令。安装默认开启固定动作同步，需要关闭时使用 `--disable-security-action-sync`。

TCPing 保留目标域名、端口、频率和超时限制。安全版移除了 web-SSH/任意命令执行能力并禁用自动更新；本文不承诺任何软件“零风险”。

## 8. 流量、重置日和查询菜单

面板综合使用 Agent 累计计数、重置日与时区，以及已有 vnStat 锚点；配置了官方流量来源或 DMIT / NoSLA 校准的节点继续使用对应配置。升级时必须保留这些锚点和校准状态，不能将重启后的网卡计数简单视为完整账单周期流量。

未设置重置日的节点没有精确的套餐日配置；建议填写真实日期，再核对面板与 Telegram 中的周期和剩余流量。不同统计来源口径可能不同，应按该节点已配置的账单规则检查。

| Telegram 命令 | 用途 |
| --- | --- |
| `/start`、`/help` | 菜单与帮助 |
| `/nodes` | 节点列表 |
| `/today`、`/yesterday` | 指定日期流量 |
| `/cycle`、`/total`、`/alltotal` | 周期及累计流量查询 |
| `/remaining` | 剩余流量 |
| `/reset`、`/resetlist` | 重置日查询 |
| `/status`、`/report` | 状态与报告 |

重置日提醒按节点自己的时区判断并记录发送状态，避免重启后重复提醒。Telegram 的 `command_timezone` 不会改变 VPS 的重置时区，也不应在升级时用它替换旧数据库的时间解释规则。

## 9. 部署验收

- 确认实际运行的镜像和页面版本与本次构建一致，清理或更新旧反向代理静态资源覆盖，浏览器刷新后仍加载正确前端。
- 确认预期在线节点重新连接，版本、v2 上报、近期指标持续更新；历史离线节点单独记录，不误判为升级故障。
- 核对节点认证、账单、流量锚点、重置日期、SSH 白名单、通知渠道和规则。
- 升级场景以冻结源与迁移结果的审计为准，确认历史数据保留；校准脚本状态也应检查，长期过期的源样本不能靠升级数据库结构解决。
- 经实际测试确认通知渠道可用后，再恢复相关定时任务。保留旧镜像和完整备份，直到新版本稳定并完成验收。
