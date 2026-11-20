#!/bin/sh
# Installs the headless RamiCore host as a systemd service.
# Run from the extracted package: sudo ./install.sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
	echo "run as root: sudo $0" >&2
	exit 1
fi

SRC=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PREFIX="${PREFIX:-/opt/ramicore}"
SERVICE_USER="${SERVICE_USER:-${SUDO_USER:-}}"
SERVICE_USER="${SERVICE_USER:-root}"

for f in ramicore web node; do
	[ -e "$SRC/$f" ] || { echo "missing $f in $SRC" >&2; exit 1; }
done

if [ "$SERVICE_USER" = root ]; then
	echo "warning: running the agent host as root; set SERVICE_USER=<user> to use a normal account" >&2
fi

install -d -m 0755 "$PREFIX"
install -m 0755 "$SRC/ramicore" "$PREFIX/ramicore"
rm -rf "$PREFIX/web" "$PREFIX/node"
cp -R "$SRC/web" "$PREFIX/web"
cp -R "$SRC/node" "$PREFIX/node"

# Keep an existing token/address on upgrade; allow an explicit override.
TOKEN="${RAMICORE_TOKEN:-}"
ADDR="${RAMICORE_ADDR:-}"
install -d -m 0750 /etc/ramicore
if [ -f /etc/ramicore/ramicore.env ]; then
	. /etc/ramicore/ramicore.env
	TOKEN="${TOKEN:-${RAMICORE_TOKEN:-}}"
	ADDR="${ADDR:-${RAMICORE_ADDR:-}}"
fi
[ -n "$TOKEN" ] || TOKEN=$(openssl rand -hex 24 2>/dev/null || od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
[ -n "$ADDR" ] || ADDR=0.0.0.0:17420
umask 077
printf 'RAMICORE_TOKEN=%s\nRAMICORE_ADDR=%s\n' "$TOKEN" "$ADDR" > /etc/ramicore/ramicore.env
chmod 600 /etc/ramicore/ramicore.env

sed -e "s|@USER@|$SERVICE_USER|g" -e "s|@PREFIX@|$PREFIX|g" \
	"$SRC/ramicore.service" > /etc/systemd/system/ramicore.service
chmod 644 /etc/systemd/system/ramicore.service

systemctl daemon-reload
systemctl enable ramicore >/dev/null 2>&1
systemctl restart ramicore
sleep 1

IP=$(hostname -I 2>/dev/null | awk '{print $1}')
echo
echo "RamiCore installed at $PREFIX and running as $SERVICE_USER."
echo "  UI:     http://${IP:-<server-ip>}:${ADDR##*:}/"
echo "  Token:  $TOKEN"
echo "  Config: /etc/ramicore/ramicore.env"
echo "  Logs:   journalctl -u ramicore -f"
echo
if ! sudo -u "$SERVICE_USER" sh -c 'command -v node >/dev/null 2>&1'; then
	echo "warning: Node.js 22+ was not found for $SERVICE_USER." >&2
	echo "         Install it (e.g. NodeSource) and restart: systemctl restart ramicore" >&2
fi
