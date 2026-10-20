#!/bin/sh
# Daily database dump (installed as /etc/cron.daily/juju-backup), keeping
# 14 days. These sit on the same disk as the database, so they protect
# against mistakes, not against losing the server: turn on Hetzner's
# server backups as well.
set -eu
dir=/var/backups/juju
mkdir -p "$dir"
chmod 700 "$dir"
cd /tmp
sudo -u postgres pg_dump -Fc juju > "$dir/juju-$(date +%F).dump"
find "$dir" -name 'juju-*.dump' -mtime +14 -delete
