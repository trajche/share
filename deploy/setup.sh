#!/usr/bin/env bash
# One-time server setup for share.mk
# Run as root on the target server: bash setup.sh
set -euo pipefail

if command -v caddy &>/dev/null; then
  echo "==> Caddy already installed ($(caddy version)), skipping."
else
  echo "==> Installing Caddy..."
  apt-get update -q
  apt-get install -y -q debian-keyring debian-archive-keyring apt-transport-https curl
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
    | gpg --batch --no-tty --yes --dearmor \
          -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
    | tee /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -q
  apt-get install -y -q caddy
fi

echo "==> Creating sharemk user..."
useradd --system --no-create-home --shell /bin/false sharemk 2>/dev/null || true

echo "==> Creating /opt/sharemk..."
mkdir -p /opt/sharemk
chown sharemk:sharemk /opt/sharemk
chmod 750 /opt/sharemk

echo "==> Installing systemd service..."
cat > /etc/systemd/system/sharemk.service <<'SERVICE'
[Unit]
Description=share.mk resumable upload service
After=network.target
Wants=network.target

[Service]
Type=simple
User=sharemk
Group=sharemk
WorkingDirectory=/opt/sharemk
EnvironmentFile=/opt/sharemk/.env
ExecStart=/opt/sharemk/sharemk
Restart=on-failure
RestartSec=5s
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/opt/sharemk

[Install]
WantedBy=multi-user.target
SERVICE
systemctl daemon-reload
systemctl enable sharemk

echo "==> Installing Caddyfile..."
cat > /etc/caddy/Caddyfile <<'CADDY'
share.mk {
    encode gzip
    reverse_proxy localhost:8080
}
CADDY
systemctl enable caddy
systemctl reload-or-restart caddy

echo ""
echo "Done. Next steps:"
echo "  1. scp .env root@share.mk:/opt/sharemk/.env"
echo "  2. make deploy"
