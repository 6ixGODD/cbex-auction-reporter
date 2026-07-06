# cbex-auction-reporter

定时抓取北交互联司法车辆项目列表，生成当日摘要，并推送到企业微信。配置 Resend 后，也可以同步发送邮件。

## 功能

- 按设定时间每天运行一次
- 抓取司法车辆项目批次、报名截止时间、竞价时间、标的数量、围观次数等信息
- 生成企业微信 Markdown 报告
- 可选发送 HTML 邮件
- 支持 Docker Compose 部署

## 本地运行

复制示例配置：

```powershell
Copy-Item .env.example .env
```

填写 `.env` 里的企业微信配置后运行：

```powershell
$env:RUN_ON_START="true"
go run .
```

## Docker 部署

```powershell
docker compose up -d --build
```

如果使用 Resend，API Key 可以写进 `.env` 的 `RESEND_API_KEY`，也可以放到 `secrets/resend_api_key`，由 Docker secret 挂载。

## 配置

程序会读取根目录 `.env`。Docker Compose 也使用根目录 `.env`。

| 变量 | 说明 | 默认值 |
| --- | --- | --- |
| `TZ` | 时区 | `Asia/Shanghai` |
| `SCHEDULE_TIME` | 每日运行时间 | `09:30` |
| `RUN_ON_START` | 启动时是否立即运行一次 | `false` |
| `CBEX_BASE_URL` | 北交互联基础地址 | `https://jpxkc.cbex.com` |
| `CBEX_LISTING_URL` | 列表页地址 | `https://jpxkc.cbex.com/page/jpxkc/zt/list` |
| `CBEX_TIMEOUT` | 请求超时 | `30s` |
| `CBEX_MAX_BATCHES` | 最多抓取批次数 | `3` |
| `WECOM_CORP_ID` | 企业微信 Corp ID | 必填 |
| `WECOM_CORP_SECRET` | 企业微信应用 Secret | 必填 |
| `WECOM_AGENT_ID` | 企业微信应用 Agent ID | 必填 |
| `WECOM_TOUSER` | 企业微信接收人 | 必填 |
| `EMAIL_ENABLED` | 是否启用邮件 | 有 Resend Key 时默认启用 |
| `EMAIL_TO` | 邮件接收人 | 邮件启用时必填 |
| `MAIL_DEFAULT_FROM` | 邮件发件人 | `onboarding@resend.dev` |
| `MAIL_DEFAULT_FROM_NAME` | 邮件发件人名称 | `CBEX Cron` |
| `RESEND_API_KEY` | Resend API Key | 可选 |
| `RESEND_API_KEY_FILE` | Resend API Key 文件路径 | `secrets/resend_api_key` |

## 免责声明

本项目只做公开网页信息的定时抓取和通知，不保证数据完整性、实时性或准确性。项目不提供任何投资、竞拍或法律建议，实际信息请以官方页面和相关公告为准。
