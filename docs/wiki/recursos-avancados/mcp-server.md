# Servidor MCP

Evolution GO puede exponer cada instancia como un servidor [MCP](https://modelcontextprotocol.io) (Model Context Protocol), para que asistentes como Claude o ChatGPT lean chats y envíen mensajes de WhatsApp.

## Activación

```env
MCP_ENABLED=true

# Opcional, pero necesario para leer conversaciones
MESSAGE_STORE_ENABLED=true
# MESSAGE_STORE_PATH=./dbdata/messages.db   # por defecto: <dir del binario>/dbdata/messages.db
# MESSAGE_STORE_RETENTION_DAYS=0            # 0 = conservar siempre
```

Ambas opciones están desactivadas por defecto. Con `MCP_ENABLED=true` el endpoint queda en `POST /mcp` (transporte Streamable HTTP, sin estado).

## Autenticación

Hay dos formas de autenticarse, y en ambas la sesión MCP solo ve una instancia.

**Token de instancia en una cabecera** (Claude Code, Claude Desktop, API). Es el token de la instancia, no la `GLOBAL_API_KEY`:

```
apikey: <token-de-instancia>
Authorization: Bearer <token-de-instancia>
```

**OAuth 2.1** (Claude.ai, ChatGPT y cualquier cliente que no permita cabeceras). El propio Evolution GO actúa como servidor de autorización:

1. El cliente recibe un `401` en `/mcp` y descubre el servidor en `/.well-known/oauth-protected-resource`.
2. Se registra solo en `/oauth/register` (registro dinámico de clientes).
3. Abre `/oauth/authorize` en el navegador, donde el usuario pega el token de la instancia y autoriza.
4. Canjea el código en `/oauth/token` (PKCE S256 obligatorio) y recibe un access token de 1 hora y un refresh token de 90 días, que rota en cada uso.

La autorización queda ligada a la instancia cuyo token se pegó y deja de funcionar si la instancia se elimina. Los clientes registrados y los hashes de los tokens se guardan en `dbdata/mcp_oauth.db` (`MCP_OAUTH_DB_PATH`), así que las conexiones sobreviven a los reinicios.

OAuth exige que la API sea accesible por **HTTPS público**. Las URLs que se anuncian salen de `MCP_PUBLIC_URL`; si no está definida, de las cabeceras `Host` / `X-Forwarded-Proto` que envía el proxy o túnel.

## Conexión

**Claude Code**

```bash
claude mcp add --transport http whatsapp https://tu-api.example.com/mcp \
  --header "Authorization: Bearer <token-de-instancia>"
```

**Claude Desktop y otros clientes con archivo de configuración**

```json
{
  "mcpServers": {
    "whatsapp": {
      "type": "http",
      "url": "https://tu-api.example.com/mcp",
      "headers": { "Authorization": "Bearer <token-de-instancia>" }
    }
  }
}
```

**Claude.ai (web, escritorio y móvil)**: Configuración → Conectores → *Agregar conector personalizado*, con la URL `https://tu-api.example.com/mcp`. Se abrirá la página de autorización para pegar el token de la instancia.

**ChatGPT**: Configuración → Conectores (requiere el modo desarrollador) → crear conector con la misma URL y autenticación OAuth.

## Exponerlo con cloudflared

Prueba rápida con una URL temporal:

```bash
cloudflared tunnel --url http://localhost:4010
```

Túnel estable con dominio propio:

```bash
cloudflared tunnel create evolution-mcp
cloudflared tunnel route dns evolution-mcp wa.example.com
```

```yaml
# ~/.cloudflared/config.yml
tunnel: <id-del-túnel>
credentials-file: /home/usuario/.cloudflared/<id-del-túnel>.json

ingress:
  # Solo lo que necesita un asistente; el resto de la API y el manager no salen a internet.
  - hostname: wa.example.com
    path: ^/(mcp|oauth/.*|\.well-known/oauth-.*)$
    service: http://localhost:4010
  - service: http_status:404
```

```bash
cloudflared tunnel run evolution-mcp
```

Con dominio fijo conviene definir `MCP_PUBLIC_URL=https://wa.example.com`. `docker/examples/docker-compose.mcp.yml` levanta la API con todo esto activado, escuchando solo en `127.0.0.1`.

## Tools

| Tool | Qué hace | Requiere almacén |
|------|----------|:---:|
| `get_status` | Estado de conexión y número vinculado | |
| `list_chats` | Chats por actividad reciente, con vista previa | ✅ |
| `get_messages` | Mensajes de un chat (paginable hacia atrás con `before`) | ✅ |
| `search_messages` | Búsqueda de texto en todos los chats o en uno | ✅ |
| `list_contacts` | Contactos de la agenda, con filtro | |
| `check_numbers` | Verifica qué números tienen WhatsApp | |
| `list_groups` | Grupos de la cuenta | |
| `get_group_info` | Nombre, descripción y participantes de un grupo | |
| `send_text` | Envía texto, opcionalmente citando un mensaje | |
| `send_media` | Envía imagen, video, audio o documento desde una URL | |
| `send_location` | Envía una ubicación | |
| `send_poll` | Envía una encuesta | |
| `react_to_message` | Reacciona a un mensaje con un emoji | |
| `mark_read` | Marca mensajes como leídos | |

Las tools de envío entregan mensajes reales. La creación y eliminación de instancias no se expone por MCP.

## Almacén de mensajes (SQLite)

Con `MESSAGE_STORE_ENABLED=true` se guarda en un archivo SQLite local el texto de los mensajes:

- Mensajes recibidos, mensajes enviados desde el teléfono y mensajes enviados por la API.
- El historial que WhatsApp sincroniza al vincular el dispositivo.
- Ediciones y eliminaciones "para todos" se reflejan en el mensaje original.

No se guarda:

- El contenido de archivos multimedia: solo el tipo, la leyenda y el nombre del archivo.
- Estados (`status@broadcast`), canales, reacciones ni votos de encuestas.
- Nada anterior a la activación, salvo lo que traiga la sincronización de historial.

Al eliminar una instancia se borran sus mensajes almacenados. El archivo contiene conversaciones privadas en texto plano: protégelo igual que la base de datos principal y define `MESSAGE_STORE_RETENTION_DAYS` si no necesitas conservarlas indefinidamente.

Este almacén es independiente de `DATABASE_SAVE_MESSAGES`, que solo registra el estado de entrega en PostgreSQL.
