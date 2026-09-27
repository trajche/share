# share.mk

Anonymous, zero-signup file sharing with automatic expiry. Files are stored on any S3-compatible backend and deleted when they expire. No database required.

**Live instance:** https://share.mk

---

## Usage

### MCP (for AI assistants)

Connect to `https://share.mk/mcp` and use the built-in tools:

| Tool | What it does |
|---|---|
| `upload_file` | Upload base64-encoded file (≈12 MB max) → returns `download_url` (share link), `direct_download_url`, `manage_url` and `management_token`. Optional `password`, `expires_in`, `disposition` |
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

# → Upload-Management-Token: {token}   (shown once — keep it)
# → Upload-Manage-URL: https://share.mk/manage/{objectId}#{token}
# → Upload-Share-URL:  https://share.mk/files/{objectId}/report.pdf   (link to share)

# 3. Download (curl gets the file; browsers may get the preview or download page)
curl https://share.mk/files/{objectId}/report.pdf -o report.pdf

# 4. Info / delete before expiry
curl https://share.mk/api/files/{id} -H "Authorization: Bearer {token}"
curl -X DELETE https://share.mk/api/files/{id} -H "Authorization: Bearer {token}"
```

Upload metadata keys (values base64-encoded):

| Key | Values |
|---|---|
| `filename` | original filename |
| `filetype` | MIME type |
| `expires-in` | `1h`, `6h`, `24h` (default), `7d`, `30d` |
| `disposition` | `inline` (default) or `attachment` (never preview; browsers get the download page) |
| `password` | optional; required to download (only a hash is stored) |

### Preview vs. download

Every upload gets two public links:

| Link | Behaviour |
|---|---|
| `/files/{objectId}/{filename}` (share link) | Images, video (with seeking), audio and PDF open in the browser. Text, HTML, SVG source and code show as plain text (UTF-8), never executed. Other files — and uploads with `disposition: attachment` — show a page with the filename, size, expiry and a Download button. Non-browser clients (curl, scripts, AI tools) always get the file itself. |
| `/dl/{objectId}/{filename}` (direct download) | Always downloads. `?dl=1` on any link does the same. |

The filename segment is cosmetic (tab titles, PDF viewer, "Save as"); `/files/{id}` works too.
Downloads carry `nosniff` and a restrictive `Content-Security-Policy`.

### Password-protected files

Browsers get a password form; after unlocking, a per-file cookie keeps it open (share link and
direct download) for 12h. API clients send the password with HTTP Basic auth:
`curl -u :secret https://share.mk/dl/{objectId}/file.zip -o file.zip`.

### Limits

- Max file size 10 GiB via tus; about 12 MB via MCP (16 MiB request cap)
- Expiry: `1h`, `6h`, `24h` (default), `7d`, `30d`; expired files are removed within ~10 minutes
- Concurrent uploads: 5 per IP / 50 overall; MCP: 2 per IP / 10 overall (HTTP 429 beyond that)
- Unfinished uploads idle for 48h are removed

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
| `SERVER_ADDR` | | `:8080` | Listen address. Behind a local reverse proxy use `127.0.0.1:8080` so the app is not reachable directly. |
| `RATE_LIMIT_GLOBAL` | | `50` | Max concurrent uploads globally |
| `RATE_LIMIT_PER_IP` | | `5` | Max concurrent uploads per IP |
| `TRUSTED_PROXIES` | | `127.0.0.1/32,::1/128` | Comma-separated IPs/CIDRs whose `X-Forwarded-For` is trusted for the client IP. Set empty to trust none. |
| `MCP_MAX_BODY_BYTES` | | `16777216` | Max MCP request body (base64 file content is decoded in memory) |
| `MCP_RATE_LIMIT_GLOBAL` | | `10` | Max concurrent MCP requests globally |
| `MCP_RATE_LIMIT_PER_IP` | | `2` | Max concurrent MCP requests per IP |
| `INCOMPLETE_UPLOAD_TTL` | | `48h` | Unfinished uploads idle longer than this are removed and their multipart uploads aborted |
| `LOG_LEVEL` | | `info` | `debug` \| `info` \| `warn` \| `error` |

### Production deployment

Pre-built binaries for Linux amd64 and arm64 are on the [releases page](https://github.com/trajche/share/releases).

The [`deploy/`](deploy/) directory contains a setup script that installs Caddy, creates a `sharemk` system user, and configures the systemd service. Run once on a fresh Debian/Ubuntu server:

```bash
make setup-server        # SSH in and run deploy/setup.sh
scp .env root@yourserver:/opt/sharemk/.env
make deploy              # cross-compile arm64 + scp + systemctl restart
```

To deploy to an amd64 server, update `BINARY_ARM64` → `BINARY_AMD64` in the `deploy` Makefile target, or just `scp` the amd64 binary manually from the [releases page](https://github.com/trajche/share/releases).

#### Service management

The systemd unit (`/etc/systemd/system/sharemk.service`) is configured to:

- **Always restart** (`Restart=always`) — recovers from crashes, OOM kills, and clean exits
- **Wait for network** (`After=network-online.target`) — ensures S3 connectivity before starting
- **Limit restart rate** (`StartLimitBurst=5` per 60 s) — prevents a crash loop from spinning
- **Restart delay** of 5 s between attempts

Both `sharemk` and `caddy` are enabled at boot (`systemctl enable`). Check their status with:

```bash
systemctl status sharemk caddy
journalctl -u sharemk -f    # follow sharemk logs
journalctl -u caddy -f      # follow caddy logs
```

---

## License

MIT
