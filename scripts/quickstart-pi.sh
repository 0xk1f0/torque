#!/bin/bash
# ABOUTME: Quickstart for the Sendspin client on a Raspberry Pi
# ABOUTME: Installs deps, builds torque, registers a systemd service
set -euo pipefail

# Usage:
#   curl -fsSL https://raw.githubusercontent.com/<fork>/torque/main/scripts/quickstart-pi.sh | sudo bash     \
#       -s -- --server 192.168.1.10:8927
# Flags (after --):
#   --server <host:port>  (required) encrypted server to connect to
#   --name    <name>      client friendly name
#   --identity <path>     identity key path (default /etc/sendspin/identity)
#   --branch  <branch>    repo branch to build (default main)
#   --repo    <url>       repo to clone (default https://github.com/0xk1f0/torque.git)
#   --uninstall           remove the service, binary, and deps

[[ "$(id -u)" -eq 0 ]] || { echo "run as root (sudo)" >&2; exit 1; }

SERVER=""
NAME=""
IDENTITY="/etc/sendspin/identity"
BRANCH="main"
REPO="https://github.com/0xk1f0/torque.git"
UNINSTALL=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --server)   SERVER="$2";   shift 2 ;;
    --name)     NAME="$2";     shift 2 ;;
    --identity) IDENTITY="$2"; shift 2 ;;
    --branch)   BRANCH="$2";   shift 2 ;;
    --repo)     REPO="$2";     shift 2 ;;
    --uninstall) UNINSTALL=1;  shift ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done

uninstall() {
  systemctl disable --now torque 2>/dev/null || true
  rm -f /etc/systemd/system/torque.service
  rm -f /usr/local/bin/torque
  systemctl daemon-reload
  echo "Removed torque. /etc/default/torque and $IDENTITY left in place."
}

install_deps() {
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update
    apt-get install -y libopus0 libasound2 pkg-config libopus-dev libasound2-dev
  else
    echo "Unsupported distro — install libopus0 + ALSA manually" >&2
  fi
}

if [[ "$UNINSTALL" == "1" ]]; then
  uninstall
  exit 0
fi

[[ -n "$SERVER" ]] || { echo "--server is required (e.g. --server 192.168.1.10:8927)" >&2; exit 1; }

echo "Installing runtime/build dependencies..."
install_deps

echo "Building torque from $REPO@$BRANCH ..."
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
git clone --depth 1 --branch "$BRANCH" "$REPO" "$TMP/repo"
(
  cd "$TMP/repo"
  make torque 2>/dev/null || GOFLAGS=-tags=nolibopusfile go build -o torque ./cmd/torque
)
install -m 755 "$TMP/repo/torque" /usr/local/bin/torque

echo "Installing systemd service..."
cat > /etc/default/torque <<EOF
# torque configuration
SENDSPIN_SERVER=$SERVER
TORQUE_OPTS=--name "$NAME" --identity $IDENTITY
EOF
install -m 644 "$TMP/repo/dist/systemd/torque.service" /etc/systemd/system/torque.service
systemctl daemon-reload

echo ""
echo "Done. Start it with:"
echo "  sudo systemctl enable --now torque"
echo "See the log with:"
echo "  journalctl -u torque -f"
