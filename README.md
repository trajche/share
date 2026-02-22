# share.mk

Anonymous, zero-signup file sharing with automatic expiry. Files are stored on any S3-compatible backend and deleted when they expire. No database required.

**Live instance:** https://share.mk

---

## Usage

### MCP (for AI assistants)

Connect to `https://share.mk/mcp` and use the built-in tools:

| Tool | What it does |
|---|---|
| `upload_file` | Upload base64-encoded file → returns `download_url` + `management_token` |
| `get_file_info` | Fetch metadata (requires `management_token`) |
| `delete_file` | Delete file (requires `management_token`) |

Full instructions at [share.mk/llms.txt](https://share.mk/llms.txt).

### curl (tus resumable uploads)

```bash
# 1. Create upload
curl -D - -X POST https://share.mk/files/ \
  -H "Tus-Resumable: 1.0.0" \
  -H "Upload-Length: $(wc -c < report.pdf)" \
  -H "Upload-Metadata: filename $(echo -n report.pdf | base64),expires-in MjRo"
# → Location: https://share.mk/files/{id}

# 2. Send bytes
curl -X PATCH "https://share.mk/files/{id}" \
  -H "Tus-Resumable: 1.0.0" \
  -H "Upload-Offset: 0" \
  -H "Content-Type: application/offset+octet-stream" \
  --data-binary @report.pdf

# 3. Download
curl https://share.mk/files/{id} -o report.pdf
```

`expires-in` options: `1h`, `6h`, `24h` (default), `7d`, `30d`.

Interactive API docs: [share.mk/docs](https://share.mk/docs)

---

## Self-hosting

### Requirements

- Go 1.23+
- An S3-compatible bucket (Scaleway, AWS, MinIO, …)
- Caddy or nginx for TLS (optional for local use)

### Quick start

```bash
git clone https://github.com/trajche/share
cd share
cp .env.example .env
# fill in S3 credentials
make run
```

### Environment variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `S3_BUCKET` | ✓ | — | Bucket name |
| `S3_REGION` | ✓ | — | Region, e.g. `fr-par` |
| `S3_ENDPOINT` | ✓ | — | S3 endpoint URL |
| `S3_ACCESS_KEY` | ✓ | — | Access key ID |
| `S3_SECRET_KEY` | ✓ | — | Secret access key |
| `S3_OBJECT_PREFIX` | | `uploads/` | Key prefix for stored objects |
| `PUBLIC_URL` | | `http://localhost:8080` | Public base URL (used in MCP download URLs) |
| `TUS_BASE_PATH` | | `/files/` | Base path for tus endpoints |
| `TUS_MAX_SIZE` | | `10737418240` | Max upload size in bytes (10 GiB) |
| `SERVER_ADDR` | | `:8080` | Listen address |
| `RATE_LIMIT_GLOBAL` | | `50` | Max concurrent uploads globally |
| `RATE_LIMIT_PER_IP` | | `5` | Max concurrent uploads per IP |
| `LOG_LEVEL` | | `info` | `debug` \| `info` \| `warn` \| `error` |

### Production deployment

Pre-built binaries for Linux amd64 and arm64 are on the [releases page](https://github.com/trajche/share/releases).

The [`deploy/`](deploy/) directory contains a systemd unit file and a setup script that installs Caddy and configures the service. Run once on a fresh server:

```bash
make setup-server        # SSH in and run deploy/setup.sh
scp .env root@yourserver:/opt/sharemk/.env
make deploy              # cross-compile + scp + systemctl restart
```

---

## License

MIT
