# Yes

Tunnel TCP traffic through Telegram (or Bale) bot messages. Built to get around firewalls.

```
You (SOCKS5) → Client → Telegram Channel → Server → Internet
```

Both sides are bots in a shared channel. Data goes back and forth as messages. That's it.

## Quick Start

You need Go and two bot tokens (one for client, one for server). Add both bots as admins in a shared Telegram channel or group.

**Server** (runs where the internet is free):

```bash
SERVER_BOT_TOKEN=... CHAT_ID=... go run ./cmd/server
```

The server connects to a SOCKS5 proxy at `127.0.0.1:1080` by default. Run one alongside it, e.g. [microsocks](https://github.com/rofl0r/microsocks).

**Client** (runs where you are):

```bash
CLIENT_BOT_TOKEN=... CHAT_ID=... go run ./cmd/client
```

Point your browser's SOCKS5 proxy to `localhost:1080`. Done.

## Options

All configured via environment variables.

| Variable | Default | What it does |
|---|---|---|
| `ENABLE_FILES` | off | Use file uploads for larger payloads (recommended) |
| `BASE_URL` | `https://api.telegram.org/bot` | Set to `https://tapi.bale.ai/bot` for Bale |
| `LISTEN_HOST` / `LISTEN_PORT` | `0.0.0.0` / `1080` | Client listen address |
| `UPSTREAM_HOST` / `UPSTREAM_PORT` | `127.0.0.1` / `1080` | Server upstream SOCKS5 address |

### Multiple Bots

Telegram rate-limits each bot to ~30 msg/sec. Pass multiple tokens comma-separated to spread the load:

```bash
CLIENT_BOT_TOKEN=tok1,tok2,tok3
```

### Webhook Mode

By default both sides use long polling. To use webhooks instead:

```bash
CLIENT_WEBHOOK_URL=https://... CLIENT_WEBHOOK_PORT=8443
SERVER_WEBHOOK_URL=https://... SERVER_WEBHOOK_PORT=8443
```

## Limitations

This is a proof of concept. Speed depends entirely on Telegram's API. It's not encrypted beyond what Telegram provides. Keep your bot tokens secret.
