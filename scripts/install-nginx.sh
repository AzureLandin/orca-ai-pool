#!/usr/bin/env bash
# Install the poolctl-generated nginx entrypoint and a self-signed TLS
# certificate when no certificate exists yet.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONF_SRC="$ROOT/nginx/conf.d/orca.conf"
SNIPPET_SRC="$ROOT/nginx/snippets/orca-proxy.conf"
CONF_DEST="/etc/nginx/conf.d/orca.conf"
SNIPPET_DEST="/etc/nginx/snippets/orca-proxy.conf"
CERT_DEST="/etc/nginx/ssl/orca.crt"
KEY_DEST="/etc/nginx/ssl/orca.key"

if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
    echo "run with sudo: sudo bash scripts/install-nginx.sh" >&2
    exit 2
fi
if ! command -v nginx >/dev/null 2>&1; then
    echo "nginx is not installed; install it first (for Debian: apt-get install nginx)" >&2
    exit 2
fi
if [[ ! -f "$CONF_SRC" || ! -f "$SNIPPET_SRC" ]]; then
    echo "generated nginx files are missing; run: python3 scripts/poolctl.py render" >&2
    exit 2
fi

install -d -m 0755 /etc/nginx/conf.d /etc/nginx/snippets /etc/nginx/ssl

backup_if_present() {
    local path="$1"
    if [[ -f "$path" ]]; then
        local backup="${path}.bak.$(date -u +%Y%m%dT%H%M%SZ)"
        cp -a "$path" "$backup"
        echo "backup: $backup"
    fi
}

backup_if_present "$CONF_DEST"
backup_if_present "$SNIPPET_DEST"

if [[ -e "$CERT_DEST" || -e "$KEY_DEST" ]] && [[ ! -s "$CERT_DEST" || ! -s "$KEY_DEST" ]]; then
    echo "incomplete TLS state: $CERT_DEST and $KEY_DEST must both exist or both be absent" >&2
    exit 2
fi
if [[ ! -s "$CERT_DEST" && ! -s "$KEY_DEST" ]]; then
    if ! command -v openssl >/dev/null 2>&1; then
        echo "openssl is required to create $CERT_DEST" >&2
        exit 2
    fi
    echo "creating self-signed certificate: $CERT_DEST"
    openssl req -x509 -nodes -newkey rsa:4096 -days 3650 \
        -keyout "$KEY_DEST" \
        -out "$CERT_DEST" \
        -subj "/CN=${ORCA_TLS_CN:-orca-ai-pool}"
    chmod 0600 "$KEY_DEST"
    chmod 0644 "$CERT_DEST"
fi

install -m 0644 "$CONF_SRC" "$CONF_DEST"
install -m 0644 "$SNIPPET_SRC" "$SNIPPET_DEST"

nginx -t
if command -v systemctl >/dev/null 2>&1; then
    if systemctl is-active --quiet nginx; then
        systemctl reload nginx
    else
        systemctl enable --now nginx
    fi
else
    nginx -s reload 2>/dev/null || nginx
fi

echo "nginx entrypoint installed: $CONF_DEST"
echo "TLS certificate: $CERT_DEST (self-signed unless you supplied one)"
