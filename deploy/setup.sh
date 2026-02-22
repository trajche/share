#!/usr/bin/env bash
# One-time server setup for share.mk
# Run as root on the target server: bash setup.sh
set -euo pipefail

echo "==> Installing Caddy..."
apt-get update -q
apt-get install -y -q debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
  | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
  | tee /etc/apt/sources.list.d/caddy-stable.list
apt-get update -q
apt-get install -y -q caddy

echo "==> Creating sharemk user..."
useradd --system --no-create-home --shell /bin/false sharemk 2>/dev/null || true

echo "==> Creating /opt/sharemk..."
mkdir -p /opt/sharemk
chown sharemk:sharemk /opt/sharemk
chmod 750 /opt/sharemk

echo "==> Installing systemd service..."
cp "$(dirname "$0")/sharemk.service" /etc/systemd/system/sharemk.service
systemctl daemon-reload
systemctl enable sharemk

echo "==> Installing Caddyfile..."
cp "$(dirname "$0")/../Caddyfile" /etc/caddy/Caddyfile
systemctl enable caddy
systemctl reload-or-restart caddy

echo ""
echo "Done. Next steps:"
echo "  1. Copy your .env to /opt/sharemk/.env"
echo "  2. Run: make deploy   (from your local machine)"
echo "  3. Run: systemctl start sharemk"
