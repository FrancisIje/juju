#!/bin/sh
# Runs ClickHouse for local development and tests, without Docker: the same
# idea as `make devdb` for Postgres. It downloads one pinned ClickHouse
# binary into ~/.cache/juju (outside the repo, shared by every checkout),
# checks its checksum, and runs it with data in the folder given (default
# .devch/ at the repo root). Delete that folder for a clean slate.
#
#   scripts/devch.sh [data-dir]        HTTP on 127.0.0.1:${CH_HTTP_PORT:-8123}
#   scripts/devch.sh --path            print the binary's path (downloads it if needed)
set -eu

VERSION=26.8.18.2
TAG=v$VERSION-lts
# macOS builds have no published checksum; these are the SHA-256 of the
# files as first downloaded, so a changed or corrupted download fails.
SHA256_MACOS_ARM64=965edbdd3bdb834f82a8c8cda497ea4d5bf0863bba1506b24545048ce138c8b6

cache=${XDG_CACHE_HOME:-$HOME/.cache}/juju
bin=$cache/clickhouse-$VERSION
base=https://github.com/ClickHouse/ClickHouse/releases/download/$TAG

download() {
	mkdir -p "$cache"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	case "$(uname -s)-$(uname -m)" in
	Darwin-arm64)
		echo "devch: downloading ClickHouse $VERSION for macOS (about 180 MB, once)" >&2
		curl -fL --progress-bar -o "$tmp/clickhouse" "$base/clickhouse-macos-aarch64"
		echo "$SHA256_MACOS_ARM64  $tmp/clickhouse" | shasum -a 256 -c - >&2
		;;
	Linux-x86_64 | Linux-aarch64)
		arch=amd64
		[ "$(uname -m)" = aarch64 ] && arch=arm64
		file=clickhouse-common-static-$VERSION-$arch.tgz
		echo "devch: downloading ClickHouse $VERSION for Linux/$arch (once)" >&2
		curl -fsSL -o "$tmp/$file" "$base/$file"
		curl -fsSL -o "$tmp/$file.sha512" "$base/$file.sha512"
		(cd "$tmp" && sha512sum -c "$file.sha512") >&2
		tar -xzf "$tmp/$file" -C "$tmp"
		mv "$(find "$tmp" -path '*/usr/bin/clickhouse' -type f | head -n 1)" "$tmp/clickhouse"
		;;
	*)
		echo "devch: no pinned ClickHouse build for $(uname -s)-$(uname -m); install ClickHouse and set JUJU_CLICKHOUSE_URL" >&2
		exit 1
		;;
	esac
	chmod +x "$tmp/clickhouse"
	mv "$tmp/clickhouse" "$bin"
}

[ -x "$bin" ] || download

if [ "${1:-}" = "--path" ]; then
	echo "$bin"
	exit 0
fi

dir=$(mkdir -p "${1:-.devch}" && cd "${1:-.devch}" && pwd)
http=${CH_HTTP_PORT:-8123}
echo "devch: ClickHouse on http://127.0.0.1:$http (data in $dir; Ctrl-C to stop)" >&2
mkdir -p "$dir/data" "$dir/tmp" "$dir/user_files" # ClickHouse expects these to exist
cd "$dir"
# Localhost only. juju uses the HTTP port; the native port is for the
# clickhouse client. ClickHouse also opens MySQL- and PostgreSQL-compatible
# ports; they're settable so two instances (dev and tests) don't collide.
exec "$bin" server -- \
	--path="$dir/data/" --tmp_path="$dir/tmp/" --user_files_path="$dir/user_files/" \
	--listen_host=127.0.0.1 --http_port="$http" --tcp_port="${CH_TCP_PORT:-9000}" \
	--mysql_port="${CH_MYSQL_PORT:-9004}" --postgresql_port="${CH_PG_PORT:-9005}" \
	--logger.console=1 --logger.level=warning
