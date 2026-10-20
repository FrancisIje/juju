#!/usr/bin/env bash
# Deploy juju to a server over SSH. Run from the repo root on your laptop.
#
#   deploy/deploy.sh setup   root@IP DOMAIN   once, on a fresh Ubuntu 24.04 server
#   deploy/deploy.sh release root@IP          build and ship the current code
#   deploy/deploy.sh demo    root@IP          build and ship the web example to demo.DOMAIN
#   deploy/deploy.sh ci-key  root@IP          let GitHub Actions deploy the production branch
#
# No domain yet? Use IP.sslip.io (e.g. 203.0.113.7.sslip.io): it resolves to
# that IP, so Caddy can still get a real HTTPS certificate.
set -euo pipefail
cd "$(dirname "$0")/.."

usage() { sed -n '4,7p' "$0" | sed 's/^# //'; exit 1; }
[ $# -ge 2 ] || usage
cmd=$1 host=$2

case $cmd in
setup)
	domain=${3:-}
	[ -n "$domain" ] || usage
	ssh "$host" 'rm -rf /tmp/juju-deploy && mkdir /tmp/juju-deploy'
	scp -q deploy/* "$host:/tmp/juju-deploy/"
	ssh "$host" "bash /tmp/juju-deploy/setup.sh '$domain' && rm -rf /tmp/juju-deploy"
	;;
release)
	# Build for the server's CPU: Hetzner's CAX plans are ARM, CX/CPX are x86.
	case $(ssh "$host" juju-receive arch) in
	aarch64) arch=arm64 ;;
	x86_64) arch=amd64 ;;
	*) echo "unsupported server CPU" >&2; exit 1 ;;
	esac
	echo "==> building dashboard"
	make dashboard
	echo "==> building juju for linux/$arch"
	mkdir -p .build
	# CGO_ENABLED=0 gives a static binary that needs nothing installed on the server.
	(cd server && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags='-s -w' -o ../.build/juju ./cmd/juju)
	echo "==> uploading"
	ssh "$host" juju-receive install < .build/juju
	;;
ci-key)
	# A fresh key for GitHub Actions, allowed to run only juju-receive.
	# Running this again replaces the old key (rotation).
	command -v gh >/dev/null || { echo "needs the GitHub CLI: brew install gh" >&2; exit 1; }
	# CI must trust the server's host key, or anyone between GitHub and the
	# server could pretend to be it. Take it from your own known_hosts.
	known=$(ssh-keygen -F "${host#*@}" | grep -v '^#') ||
		{ echo "run 'ssh $host' once first, so its host key is in ~/.ssh/known_hosts" >&2; exit 1; }
	dir=$(mktemp -d)
	trap 'rm -rf "$dir"' EXIT
	ssh-keygen -q -t ed25519 -N '' -C github-actions-deploy -f "$dir/key"
	ssh "$host" "install -d -m 700 ~/.ssh && touch ~/.ssh/authorized_keys &&
		sed -i '/ github-actions-deploy\$/d' ~/.ssh/authorized_keys &&
		echo 'restrict,command=\"/usr/local/bin/juju-receive\" $(cat "$dir/key.pub")' >> ~/.ssh/authorized_keys"
	# Secrets live in the "production" environment: only jobs that declare
	# `environment: production` can read them.
	gh api -X PUT 'repos/{owner}/{repo}/environments/production' > /dev/null
	gh secret set DEPLOY_SSH_KEY --env production < "$dir/key"
	gh secret set DEPLOY_KNOWN_HOSTS --env production --body "$known"
	gh secret set DEPLOY_HOST --env production --body "$host"
	echo "==> done: pushing to the production branch now deploys"
	;;
demo)
	# The example page loads ../../sdks/web/dist/juju.min.js and
	# dist/app.min.js, so the site keeps that folder layout.
	echo "==> building the web SDK and the minified example"
	(cd sdks && npm ci && npm run build)
	make example-web-build
	rm -rf .build/demo
	mkdir -p .build/demo/examples/web/dist .build/demo/sdks/web/dist
	cp examples/web/index.html .build/demo/examples/web/
	cp examples/web/dist/app.min.js .build/demo/examples/web/dist/
	cp sdks/web/dist/juju.min.js .build/demo/sdks/web/dist/
	# The source map is not published: visitors would get your source. It
	# goes to juju instead, which uses it to symbolicate the stack traces.
	echo "==> uploading"
	tar -C .build/demo -czf - . | ssh "$host" 'rm -rf /var/www/juju-demo/* && tar -C /var/www/juju-demo -xzf - && chmod -R a+rX /var/www/juju-demo'
	if [ -n "${JUJU_URL:-}" ] && [ -n "${JUJU_PROJECT:-}" ] && [ -n "${JUJU_SECRET_KEY:-}" ]; then
		echo "==> uploading the source map to juju"
		(cd server && go run ./cmd/juju upload -kind sourcemap -release example@0.1.0 ../examples/web/dist)
	else
		echo "==> skipped the source map: set JUJU_URL, JUJU_PROJECT and JUJU_SECRET_KEY to upload it"
	fi
	echo "==> done: open https://demo.<your domain>/ and paste the project's DSN"
	;;
*) usage ;;
esac
