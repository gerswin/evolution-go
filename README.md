<p align="center">
  <a href="https://evolutionfoundation.com.br">
    <img src="./public/hover-evolution.png" alt="Evolution Foundation" />
  </a>
</p>

<h1 align="center">Evolution Go</h1>

<p align="center">
  High-performance WhatsApp API built in Go — part of the Evolution Foundation ecosystem.
</p>

<p align="center">
  <a href="https://github.com/evolution-foundation/evolution-go/releases/latest"><img src="https://img.shields.io/github/v/release/evolution-foundation/evolution-go?include_prereleases&label=version&color=00ffa7" alt="Latest version" /></a>
  <a href="https://opensource.org/licenses/Apache-2.0"><img src="https://img.shields.io/badge/License-Apache%202.0-blue.svg" alt="License: Apache 2.0" /></a>
  <a href="https://docs.evolutionfoundation.com.br"><img src="https://img.shields.io/badge/Docs-evolutionfoundation.com.br-00ffa7" alt="Documentation" /></a>
  <a href="https://evolutionfoundation.com.br/community"><img src="https://img.shields.io/badge/Community-Join%20us-white" alt="Community" /></a>
  <a href="https://hub.docker.com/r/evoapicloud/evolution-go"><img src="https://img.shields.io/badge/Docker-evoapicloud-blue" alt="Docker image" /></a>
</p>

<p align="center">
  <a href="https://evolutionfoundation.com.br">Website</a> &middot;
  <a href="https://docs.evolutionfoundation.com.br">Documentation</a> &middot;
  <a href="https://evolutionfoundation.com.br/community">Community</a> &middot;
  <a href="mailto:suporte@evofoundation.com.br">Support</a>
</p>


---

## New in this fork: MCP server, OAuth and message history

This fork adds a [Model Context Protocol](https://modelcontextprotocol.io) server, so assistants such as **Claude** or **ChatGPT** can read your WhatsApp chats and send messages through an instance. Everything is opt-in and off by default.

- **MCP server at `/mcp`** — Streamable HTTP, 14 tools: `get_status`, `list_chats`, `get_messages`, `search_messages`, `list_contacts`, `check_numbers`, `list_groups`, `get_group_info`, `send_text`, `send_media`, `send_location`, `send_poll`, `react_to_message`, `mark_read`. Each session only sees one instance.
- **Built-in OAuth 2.1 server** — dynamic client registration, authorization code with PKCE and rotating refresh tokens, so Claude.ai and ChatGPT custom connectors work without extra infrastructure. The user pastes the instance token once on a consent page. Clients that can send headers (Claude Code, Claude Desktop) can use the instance token directly.
- **SQLite message store** — keeps the text of received and sent messages, plus the history WhatsApp syncs when a device is linked, so conversations can be listed, read and searched. Media files are not stored, only their type, caption and file name.
- **Docker Compose example** — [`docker/examples/docker-compose.mcp.yml`](./docker/examples/docker-compose.mcp.yml) starts the API with all of the above enabled.

### Enable it

```env
MCP_ENABLED=true
MESSAGE_STORE_ENABLED=true                    # required for the chat history tools
MCP_PUBLIC_URL=https://your-api.example.com   # public https origin, needed for OAuth
```

### Connect an assistant

**Claude.ai / ChatGPT** — add a custom connector with the URL `https://your-api.example.com/mcp`, then paste the instance token on the authorization page.

**Claude Code**

```bash
claude mcp add --transport http whatsapp https://your-api.example.com/mcp \
  --header "Authorization: Bearer <instance-token>"
```

OAuth needs the API reachable over public HTTPS. With a tunnel or reverse proxy, only `/mcp`, `/oauth/*` and `/.well-known/oauth-*` have to be exposed; the rest of the API and the manager can stay private.

The sending tools deliver real messages, and the message store holds private conversations in plain text, so protect the instance tokens and the `dbdata` directory accordingly.

Full guide (in Spanish), including a cloudflared setup: [docs/wiki/recursos-avancados/mcp-server.md](./docs/wiki/recursos-avancados/mcp-server.md).

---

## About

**Evolution Go** is a high-performance WhatsApp API built in Go. Part of the Evolution Foundation ecosystem, it provides a robust, lightweight solution for WhatsApp integration using the [whatsmeow](https://github.com/tulir/whatsmeow) library.

## Part of the Evolution Foundation ecosystem

Evolution Go is one of the messaging engines maintained by Evolution Foundation. It is used as a WhatsApp provider by the [Evo CRM Community](https://github.com/evolution-foundation/evo-crm-community) and other projects in the ecosystem.

---

## Features

- **High performance** — built with Go for minimal resource usage
- **RESTful API** — clean, well-documented REST endpoints with Swagger
- **Real-time events** — WebSocket, Webhook, AMQP/RabbitMQ and NATS support
- **Media support** — images, videos, audio, documents with MinIO/S3 storage
- **Message storage** — optional PostgreSQL persistence
- **QR code pairing** — built-in QR code generation for device linking
- **License management** — built-in licensing with registration, activation, and heartbeat
- **Docker ready** — production-ready Docker configuration

---

## Quick Start

### Docker (recommended)

```bash
git clone https://github.com/evolution-foundation/evolution-go.git
cd evolution-go
make docker-build
make docker-run
```

### Local development

```bash
git clone https://github.com/evolution-foundation/evolution-go.git
cd evolution-go

# Setup, configure and run
make setup
cp .env.example .env
make dev
```

> Run `make help` to see all available commands. See [COMMANDS.md](./COMMANDS.md) for detailed workflows.

---

## Configuration

Create a `.env` file:

```env
# Server
SERVER_PORT=8080
CLIENT_NAME=evolution

# Security
GLOBAL_API_KEY=your-secure-api-key-here

# Database
POSTGRES_AUTH_DB=postgresql://postgres:password@localhost:5432/evogo_auth?sslmode=disable
POSTGRES_USERS_DB=postgresql://postgres:password@localhost:5432/evogo_users?sslmode=disable
DATABASE_SAVE_MESSAGES=false

# Logging
WADEBUG=DEBUG
LOGTYPE=console

# Optional
# AMQP_URL=amqp://guest:guest@localhost:5672/
# NATS_URL=nats://localhost:4222
# WEBHOOK_URL=https://your-webhook-url.com/webhook
# MINIO_ENABLED=true
# MINIO_ENDPOINT=localhost:9000
# MINIO_ACCESS_KEY=minioadmin
# MINIO_SECRET_KEY=minioadmin
```

| Variable | Description | Default |
|---|---|---|
| `SERVER_PORT` | Server port | `8080` |
| `CLIENT_NAME` | Client identifier | `evolution` |
| `GLOBAL_API_KEY` | API authentication key | **Required** |
| `DATABASE_SAVE_MESSAGES` | Enable message storage | `false` |
| `WADEBUG` | WhatsApp debug level | `INFO` |

---

## License Activation

Evolution Go requires a license to operate. On first run:

1. Start the server — API endpoints return `503` until activated
2. Open the **Manager** at `http://localhost:8080/manager/login`
3. Enter your API URL and `GLOBAL_API_KEY`
4. Complete the license registration flow
5. Once activated, the API is fully operational

The license status persists in the database (`runtime_configs` table). Heartbeats are sent periodically to maintain activation.

---

## API Documentation

Swagger UI available at:

```
http://localhost:8080/swagger/index.html
```

### Key Endpoints

| Method | Endpoint | Description |
|---|---|---|
| `POST` | `/instance/create` | Create WhatsApp instance |
| `GET` | `/instance/{name}/qrcode` | Get QR code for pairing |
| `POST` | `/message/sendText` | Send text message |
| `POST` | `/message/sendMedia` | Send media message |
| `GET` | `/instance/{name}/status` | Get instance status |
| `DELETE` | `/instance/{name}` | Delete instance |

---

## Project Structure

```
evolution-go/
├── cmd/evolution-go/     # Application entry point
├── pkg/
│   ├── core/            # License management & middleware
│   ├── instance/        # Instance management
│   ├── message/         # Message handling
│   ├── sendMessage/     # Message sending
│   ├── routes/          # HTTP routes
│   ├── middleware/      # Auth & validation middleware
│   ├── config/          # Configuration
│   ├── events/          # Event producers (AMQP, NATS, Webhook, WS)
│   └── storage/         # Media storage (MinIO)
├── docs/                # Swagger documentation
├── Dockerfile
├── Makefile
└── VERSION
```

---

## Tech Stack

| Component | Technology |
|---|---|
| Language | Go 1.24+ |
| HTTP framework | Gin |
| WhatsApp | [whatsmeow](https://github.com/tulir/whatsmeow) |
| Database | PostgreSQL |
| ORM | GORM |
| Message queue | RabbitMQ, NATS |
| Object storage | MinIO/S3 |
| Documentation | Swagger/OpenAPI |
| Container | Docker |

---

## Documentation

| Resource | Link |
|---|---|
| Website | [evolutionfoundation.com.br](https://evolutionfoundation.com.br) |
| Documentation | [docs.evolutionfoundation.com.br](https://docs.evolutionfoundation.com.br) |
| Community | [evolutionfoundation.com.br/community](https://evolutionfoundation.com.br/community) |
| Docker Hub | [evoapicloud/evolution-go](https://hub.docker.com/r/evoapicloud/evolution-go) |
| Changelog | [CHANGELOG.md](./CHANGELOG.md) |
| Contributing | [CONTRIBUTING.md](./CONTRIBUTING.md) |
| Security | [SECURITY.md](./SECURITY.md) |

---

## Hosting

Deploy Evolution Go with optimized infrastructure through our HostGator partnership:

[**Evolution Go VPS — HostGator**](https://evolution-api.com/vps-evolution-go)

---

## Telemetry

Evolution Go collects anonymous telemetry data (routes used, API version) to help improve the service. **No sensitive or personal data is collected.**

---

## Contributing

Contributions are welcome! Please read [CONTRIBUTING.md](./CONTRIBUTING.md) for guidelines on how to submit issues, propose features, and open pull requests.

Join our [community](https://evolutionfoundation.com.br/community) to discuss ideas and collaborate.

---

## Security

For security issues, **do not open a public issue**. Email **suporte@evofoundation.com.br** or use GitHub's private vulnerability reporting. See [SECURITY.md](./SECURITY.md) for details.

---

## Acknowledgments

- [whatsmeow](https://github.com/tulir/whatsmeow) by [Tulir Asokan](https://github.com/tulir) — WhatsApp protocol library
- [Evolution API](https://github.com/evolution-foundation/evolution-api) — Node.js sister project

---

## License

Evolution Go is licensed under the Apache License 2.0, with additional brand-protection conditions (LOGO/copyright preservation and Usage Notification requirement). See [LICENSE](./LICENSE) for full details.

For licensing inquiries, contact **suporte@evofoundation.com.br**.

## Trademarks

"Evolution Foundation", "Evolution" and "Evolution Go" are trademarks of Evolution Foundation. See [TRADEMARKS.md](./TRADEMARKS.md) for the brand assets policy.

Third-party attributions are documented in [NOTICE](./NOTICE).

---

<p align="center">
  Made by <a href="https://evolutionfoundation.com.br">Evolution Foundation</a> · © 2026
</p>
