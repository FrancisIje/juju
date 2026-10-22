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

echo "==> ClickHouse, for product analytics"
# The same version as development and CI (scripts/devch.sh), from
# ClickHouse's own signed apt repository.
CLICKHOUSE_VERSION=26.8.18.2
if ! dpkg -s clickhouse-server 2>/dev/null | grep -q "^Version: $CLICKHOUSE_VERSION"; then
	curl -fsSL https://packages.clickhouse.com/rpm/lts/repodata/repomd.xml.key | gpg --dearmor --yes -o /usr/share/keyrings/clickhouse-keyring.gpg
	echo "deb [signed-by=/usr/share/keyrings/clickhouse-keyring.gpg] https://packages.clickhouse.com/deb stable main" > /etc/apt/sources.list.d/clickhouse.list
	apt-get update -q
	apt-get install -yq "clickhouse-server=$CLICKHOUSE_VERSION" "clickhouse-client=$CLICKHOUSE_VERSION" "clickhouse-common-static=$CLICKHOUSE_VERSION"
fi
install -m 644 clickhouse.xml /etc/clickhouse-server/config.d/juju.xml
systemctl enable clickhouse-server
systemctl restart clickhouse-server

echo "==> firewall: only SSH, HTTP and HTTPS are reachable"
ufw allow OpenSSH
ufw allow 80/tcp
ufw allow 443/tcp
ufw --force enable

echo "==> ssh: keys only"
# Bots try passwords on every server on the internet all day; a key can't
# be guessed. OpenSSH allows passwords by default, even when no account
# has one yet. Turned off only once a key is installed, so this can't lock
# you out. OpenSSH uses the first value it reads, and some cloud images
# ship a 50-cloud-init.conf that turns passwords on, so this file's name
# sorts before it.
if [ -s /root/.ssh/authorized_keys ]; then
	install -d -m 755 /etc/ssh/sshd_config.d
	cat > /etc/ssh/sshd_config.d/10-juju.conf <<'EOF'
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
EOF
	sshd -t && systemctl reload ssh
fi

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
# Product analytics (ClickHouse runs on this server, localhost only).
JUJU_CLICKHOUSE_URL=http://127.0.0.1:8123
# Anyone can create an account and their own organization; teams add people
# with invite links. For a private server, where invite links are the only
# way in, uncomment this:
#JUJU_ALLOW_SIGNUP=false
# Email alerts: fill these in, then run: systemctl restart juju
#JUJU_SMTP_ADDR=smtp.example.com:587
#JUJU_SMTP_USER=
#JUJU_SMTP_PASSWORD=
#JUJU_SMTP_FROM=juju <alerts@$DOMAIN>
# Limits and data (defaults shown). Per-project limits: juju-admin project limits
#JUJU_RATE_LIMIT=1000              # events/s per project; 0 = no default limit
#JUJU_RETENTION_DAYS=90            # error events, sessions, transactions; 0 = keep
#JUJU_ANALYTICS_RETENTION_DAYS=365 # ClickHouse analytics events; 0 = keep
#JUJU_SCRUB_PII=true               # remove emails, URL tokens etc. at ingest
#JUJU_TRUSTED_PROXIES=127.0.0.1,::1 # proxies whose X-Forwarded-For is believed (Caddy)
# The event queue is embedded and kept in JUJU_DATA_DIR/queue (max 1 GB).
#JUJU_QUEUE_MAX_MB=1024
EOF
fi
# Servers set up before analytics existed: turn it on, once.
grep -q '^#\?JUJU_CLICKHOUSE_URL=' /etc/juju/juju.env || echo 'JUJU_CLICKHOUSE_URL=http://127.0.0.1:8123' >> /etc/juju/juju.env
# Keep the URL in step with DOMAIN when re-run for a new domain; the rest
# of the file may hold edits (SMTP) that must survive.
sed -i "s|^JUJU_PUBLIC_URL=.*|JUJU_PUBLIC_URL=https://$DOMAIN|" /etc/juju/juju.env
chown root:juju /etc/juju/juju.env
chmod 640 /etc/juju/juju.env

echo "==> services"
install -m 644 juju.service /etc/systemd/system/juju.service
sed "s/DOMAIN {/$DOMAIN {/" Caddyfile > /etc/caddy/Caddyfile
# The web example's files (deploy.sh demo puts them here).
install -d -m 755 /var/www/juju-demo
install -m 755 juju-admin /usr/local/bin/juju-admin
install -m 755 juju-receive /usr/local/bin/juju-receive
install -m 755 backup.sh /etc/cron.daily/juju-backup
systemctl daemon-reload
systemctl enable juju
# Restarts only if already running, so it picks up a changed juju.env.
systemctl try-restart juju
systemctl reload-or-restart caddy

echo "==> done. Next, from your laptop: deploy/deploy.sh release <host>"
echo "    The web example will be at https://demo.$DOMAIN after deploy/deploy.sh demo <host>"
