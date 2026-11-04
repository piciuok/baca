#!/bin/sh
# Installs baca, enables start at login and starts it:
#   curl -fsSL https://raw.githubusercontent.com/piciuok/baca/main/scripts/install.sh | sh
#
# The binary comes from the latest GitHub release, checked against its checksums.txt, and goes
# to ~/.local/bin (no sudo). On Linux it offers the tools the activity probes use (xdotool,
# xprintidle, wmctrl, notify-send; this one step uses sudo). Questions are read from the
# terminal even when the script is piped into sh.
#
# Options (environment):
#   BACA_VERSION=v0.1.0   pin a version
#   BACA_YES=1            answer every question with its default
#   BACA_NO_DEPS=1        do not offer the Linux tools
#   BACA_NO_START=1       only put the binary in place
set -eu

repo="piciuok/baca"
bindir="${HOME}/.local/bin"
user_path="$PATH"

say() { printf '\n==> %s\n' "$*"; }
warn() { printf 'baca: %s\n' "$*" >&2; }
die() { warn "$*"; exit 1; }
has() { command -v "$1" >/dev/null 2>&1; }

# a terminal to ask on: stdin is the script itself when piped into sh
tty=""
if [ -z "${BACA_YES:-}" ] && (exec </dev/tty) 2>/dev/null; then tty=/dev/tty; fi
# ask "question" default(y|n): returns 0 for yes
ask() {
  if [ -z "$tty" ]; then [ "$2" = y ]; return; fi
  hint="[y/N]"; [ "$2" = y ] && hint="[Y/n]"
  printf '%s %s ' "$1" "$hint" >"$tty"
  read -r ans <"$tty" || ans=""
  case "${ans:-$2}" in [yY]*) return 0 ;; *) return 1 ;; esac
}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in darwin|linux) ;; *) die "unsupported OS: $os" ;; esac
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) die "unsupported architecture: $arch" ;;
esac
has curl || die "curl is needed"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

sha256() {
  if has sha256sum; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi
}
# verify file checksums.txt: the line for the file's name must match
verify() {
  want=$(grep " \*\{0,1\}$(basename "$1")\$" "$2" | awk '{print $1}' | head -n1)
  [ -n "$want" ] && [ "$want" = "$(sha256 "$1")" ] || die "checksum mismatch for $(basename "$1") — aborting"
}

# ---------------------------------------------------------------- baca
tag="${BACA_VERSION:-}"
[ -n "$tag" ] || tag=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)
[ -n "$tag" ] || die "could not find the latest release"
ver="${tag#v}"
name="baca_${ver}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/$tag"

say "baca $tag ($os/$arch)"
curl -fsSL -o "$tmp/$name" "$base/$name"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"
verify "$tmp/$name" "$tmp/checksums.txt"
tar -C "$tmp" -xzf "$tmp/$name"
mkdir -p "$bindir"
install -m 0755 "$tmp/${name%.tar.gz}/baca" "$bindir/baca.new" && mv "$bindir/baca.new" "$bindir/baca"
echo "Installed $bindir/baca ($("$bindir/baca" version))"

# ---------------------------------------------------------------- Linux tools for the probes
syspkg() {
  if has apt-get; then sudo apt-get install -y "$@"
  elif has dnf; then sudo dnf install -y "$@"
  elif has pacman; then sudo pacman -S --needed --noconfirm "$@"
  elif has zypper; then sudo zypper install -y "$@"
  else return 1
  fi
}
if [ "$os" = linux ] && [ -z "${BACA_NO_DEPS:-}" ]; then
  missing=""
  for b in xdotool xprintidle wmctrl notify-send; do has "$b" || missing="$missing $b"; done
  if [ -n "$missing" ]; then
    say "Activity tools"
    echo "The system probe reads the active window (xdotool), idle time (xprintidle) and call windows (wmctrl);"
    echo "notify-send shows update notices. Missing:$missing"
    [ -n "${WAYLAND_DISPLAY:-}" ] && [ -z "${DISPLAY:-}" ] && warn "pure Wayland: other apps cannot see the active window; only the git, agent and remote probes will work"
    if ask "Install them with your package manager (sudo)?" y; then
      pkgs="xdotool xprintidle wmctrl"
      if has apt-get; then pkgs="$pkgs libnotify-bin"; else pkgs="$pkgs libnotify"; fi
      # shellcheck disable=SC2086
      syspkg $pkgs || warn "install$missing with your package manager"
    else
      echo "  skipped"
    fi
  fi
fi

# ---------------------------------------------------------------- start at login, start now
if [ -z "${BACA_NO_START:-}" ]; then
  say "Start at login"
  "$bindir/baca" install
  "$bindir/baca" start
  echo "baca is running: the ring in the menu bar / tray. Open the UI with: baca open"
  if [ "$os" = darwin ]; then
    echo "Window titles need the Accessibility permission for $bindir/baca"
    echo "(System Settings → Privacy & Security → Accessibility). Without it baca records app names only."
  fi
fi
case ":$user_path:" in *":$bindir:"*) ;; *) echo; echo "Add $bindir to your PATH to run baca from a shell." ;; esac
