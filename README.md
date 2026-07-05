# cron-cbex

Single Go application that fetches CBEX judicial vehicle listings and sends a daily WeCom markdown report. If a Resend API key is configured, it also sends the report to email.

## Run Locally

```powershell
$env:RUN_ON_START="true"
go run .
```

## Deploy

```powershell
docker compose up -d --build
```

## Configuration

The app loads `.env` and `jobs/.env` if present. Docker Compose uses only root `.env`; keep deployment values there.

| Variable | Default |
| --- | --- |
| `TZ` | `Asia/Shanghai` |
| `SCHEDULE_TIME` | `09:30` |
| `RUN_ON_START` | `false` |
| `CBEX_BASE_URL` | `https://jpxkc.cbex.com` |
| `CBEX_LISTING_URL` | `https://jpxkc.cbex.com/page/jpxkc/zt/list` |
| `CBEX_TIMEOUT` | `30s` |
| `CBEX_MAX_BATCHES` | `3` |
| `WECOM_CORP_ID` | required |
| `WECOM_CORP_SECRET` | required |
| `WECOM_AGENT_ID` | required |
| `WECOM_TOUSER` | required |
| `EMAIL_ENABLED` | `true` when Resend key exists |
| `EMAIL_TO` | required when email is enabled |
| `MAIL_DEFAULT_FROM` | `onboarding@resend.dev` |
| `MAIL_DEFAULT_FROM_NAME` | `CBEX Cron` |
| `RESEND_API_KEY` | optional |
| `RESEND_API_KEY_FILE` | `secrets/resend_api_key` locally, `/run/secrets/resend_api_key` in Docker |
