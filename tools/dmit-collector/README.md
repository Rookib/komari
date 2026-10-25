# Komari DMIT Official Traffic Collector

这个采集器跑在已经手动通过 DMIT Cloudflare / 登录验证的 Windows 机器上。它不会绕过 Cloudflare，只复用本机浏览器 Profile 的正常登录状态，把官方流量快照上报给 Komari。

## 1. 安装

```powershell
cd tools\dmit-collector
npm install
copy config.example.json config.json
```

默认使用你电脑已安装的 Google Chrome（`browser_channel: "chrome"`）。不需要装两个浏览器；两个 DMIT 账号会分别使用两个独立 Profile 目录。

如果这台 Windows 没装 Chrome，再执行：

```powershell
npm run install-browser
```

并把 `config.json` 里的 `browser_channel` 改成空字符串或删除。

## 2. 面板配置

在 Komari 面板数据库的 `official_traffic_sources` 配置里，为每个 DMIT 节点添加独立 `collector_key`：

```json
{
  "节点UUID-1": {
    "provider": "dmit-cache",
    "enabled": true,
    "display_name": "DMIT 官方缓存 - MALIBU",
    "collector_key": "随机长密钥1",
    "cache_ttl_seconds": 300
  },
  "节点UUID-2": {
    "provider": "dmit-cache",
    "enabled": true,
    "display_name": "DMIT 官方缓存 - EB.CORONA",
    "collector_key": "随机长密钥2",
    "cache_ttl_seconds": 300
  }
}
```

`collector_key` 要和 `config.json` 中对应账号一致。不要把这个密钥发到公开地方。

## 3. 两个 DMIT 账号分别验证

每个账号使用同一个 Chrome 程序，但独立浏览器 Profile，避免串号：

```powershell
node collector.js --verify dmit-malibu
node collector.js --verify dmit-corona
```

浏览器打开后，手动登录并通过 Cloudflare，然后关闭窗口。

## 4. 测试采集

```powershell
node collector.js --once
```

如果页面结构无法自动识别流量字段，采集器会向 Komari 上报“需要人工验证/调整规则”，Komari 会通过现有通知渠道提醒你。

## 5. 安装计划任务

```powershell
powershell -ExecutionPolicy Bypass -File .\install-windows-task.ps1 -IntervalMinutes 30
```

默认每 30 分钟采集一次。

## 说明

- 不修改 agent。
- 不绕过 Cloudflare。
- 不保存 DMIT 密码。
- Cookie / 登录态只在本机 Playwright Profile 内。
- 面板显示和 TG 查询会优先使用官方缓存；官方缓存不可用时回退探针数据。
