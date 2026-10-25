# V.PS 官方流量 API 接入

Komari 现在支持通过 V.PS（`vps.hosting`）官方 User API 查询服务流量，不需要浏览器采集或 Cloudflare 验证。

接口使用：

```text
GET https://vps.hosting/api/service/{service_id}/bandwidth
```

认证支持两种方式：

- `token`：User API 生成的 Token，使用 Bearer Token；
- `username` + `password`：V.PS User API 账号密码，使用 HTTP Basic Auth。

在 Komari 的 `official_traffic_sources` 配置中，为对应探针 UUID 添加一项。例如使用 Token：

```json
{
  "探针UUID": {
    "provider": "vps-hosting",
    "enabled": true,
    "service_id": "12345",
    "token": "替换为你的VPS.hosting API Token",
    "display_name": "V.PS 官方流量",
    "cache_ttl_seconds": 120
  }
}
```

使用账号密码时，把 `token` 换成：

```json
"username": "你的 V.PS User API 用户名",
"password": "你的 V.PS User API 密码"
```

V.PS 返回的字节数字、带单位字符串（例如 `1.5 GB`）以及嵌套在 `data`、`bandwidth` 或 `usage` 下的常见字段都可以直接解析。若接口返回的是不带单位的数字且单位不是字节，可增加：

```json
"traffic_unit": "GB"
```

默认请求地址是 `https://vps.hosting/api`；如果使用兼容入口，可通过 `endpoint` 覆盖。Telegram 的 `/remaining` 会显示官方已用、套餐上限和剩余流量。
