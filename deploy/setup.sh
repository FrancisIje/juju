#!/usr/bin/env bash
# One-time setup of a fresh Ubuntu 24.04 server for juju. Don't run this by
# hand: `deploy/deploy.sh setup root@IP DOMAIN` copies this directory to the
# server and runs it there as root. Safe to run again.
set -euo pipefail
cd "$(dirname "$0")"
DOMAIN=${1:?usage: setup.sh DOMAIN}
export DEBIAN_FRONTEND=noninteractive

echo "==> packages"
apt-get update -q
apt-get upgrade -yq
# unattended-upgrades installs security fixes automatically.
apt-get install -yq postgresql ufw unattended-upgrades curl gpg debian-keyring debian-archive-keyring apt-transport-https
if ! command -v caddy >/dev/null; then
	curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
	curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt > /etc/apt/sources.list.d/caddy-stable.list
	apt-get update -q
	apt-get install -yq caddy
fi

echo "==> firewall: only SSH, HTTP and HTTPS are reachable"
ufw allow OpenSSH
ufw allow 80/tcp
ufw allow 443/tcp
ufw --force enable

echo "==> juju user and database"
id juju >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin juju
# The Postgres role has the same name as the Linux user, so juju logs in over
# the local socket with "peer" auth: Postgres asks the kernel which user is
# connecting. No password exists, so none can leak.
cd /tmp
sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='juju'" | grep -q 1 || sudo -u postgres createuser juju
sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='juju'" | grep -q 1 || sudo -u postgres createdb -O juju juju
cd - >/dev/null

echo "==> config"
install -d -m 750 -o root -g juju /etc/juju
if [ ! -f /etc/juju/juju.env ]; then
	# 127.0.0.1: juju is only reachable through Caddy, never directly.
	cat > /etc/juju/juju.env <<EOF
JUJU_ADDR=127.0.0.1:8080
JUJU_DATABASE_URL=postgres://juju@/juju?host=/var/run/postgresql
JUJU_DATA_DIR=/var/lib/juju
JUJU_PUBLIC_URL=https://$DOMAIN
# Email alerts: fill these in, then `systemctl restart juju`.
#JUJU_SMTP_ADDR=smtp.example.com:587
#JUJU_SMTP_USER=
#JUJU_SMTP_PASSWORD=
#JUJU_SMTP_FROM=juju <alerts@$DOMAIN>
EOF
fi
chown root:juju /etc/juju/juju.env
chmod 640 /etc/juju/juju.env

echo "==> services"
install -m 644 juju.service /etc/systemd/system/juju.service
sed "s/DOMAIN {/$DOMAIN {/" Caddyfile > /etc/caddy/Caddyfile
# The web example's files (deploy.sh demo puts them here).
install -d -m 755 /var/www/juju-demo
install -m 755 juju-admin /usr/local/bin/juju-admin
install -m 755 backup.sh /etc/cron.daily/juju-backup
systemctl daemon-reload
systemctl enable juju
systemctl reload-or-restart caddy

echo "==> done. Next, from your laptop: deploy/deploy.sh release <host>"
echo "    The web example will be at https://demo.$DOMAIN after deploy/deploy.sh demo <host>"
