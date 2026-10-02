#!/bin/sh
# Собирает deb-пакет wb-telegram под указанную архитектуру контроллера.
#
# Запуск внутри Debian-окружения с Go 1.26, debhelper и кросс-binutils
# (binutils-aarch64-linux-gnu, binutils-arm-linux-gnueabihf) — CI или `make deb`:
#   scripts/build-deb.sh <arm64|armhf> [версия]
# Готовый пакет кладётся в dist/wb-telegram_<версия>_<арх>.deb.
#
# arm64 — Wiren Board 8, armhf — Wiren Board 6 и 7.
set -eu

arch=${1:?usage: build-deb.sh <arm64|armhf> [version]}
version=${2:-}

case "$arch" in
arm64 | armhf) ;;
*)
	echo "unsupported architecture: $arch (expected arm64 or armhf)" >&2
	exit 2
	;;
esac

cd "$(dirname "$0")/.."

if [ -n "$version" ]; then
	# Версия пакета и бинаря задаётся снаружи (CI); changelog правится только на время сборки.
	# Копия вне debian/: dh_clean удаляет *.orig внутри пакета.
	backup=$(mktemp)
	cp debian/changelog "$backup"
	trap 'cp "$backup" debian/changelog; rm -f "$backup"' EXIT
	sed -i "1s/(.*)/($version)/" debian/changelog
	export VERSION="$version"
fi

dpkg-buildpackage -b -a"$arch" -us -uc -d
debian/rules clean

pkg_version=$(dpkg-parsechangelog -SVersion)

mkdir -p dist
mv "../wb-telegram_${pkg_version}_${arch}.deb" dist/
rm -f "../wb-telegram_${pkg_version}_${arch}.buildinfo" "../wb-telegram_${pkg_version}_${arch}.changes"

echo "dist/wb-telegram_${pkg_version}_${arch}.deb"
