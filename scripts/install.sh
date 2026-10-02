#!/bin/sh
# Ставит или обновляет wb-telegram на контроллере Wiren Board из релизов GitHub.
#
# Запуск на контроллере (под root):
#   curl -fsSL https://github.com/Format-C-eft/wb-telegram/releases/latest/download/install.sh | sh
#
# Архитектура определяется сама: arm64 — WB8, armhf — WB6 и WB7.
# Конкретная версия: WB_TELEGRAM_VERSION=v0.1.12 sh install.sh
set -eu

repo="Format-C-eft/wb-telegram"
version=${WB_TELEGRAM_VERSION:-latest}

if [ "$(id -u)" -ne 0 ]; then
	echo "Запустите под root (на контроллере Wiren Board — ssh root@<ip-контроллера>)." >&2
	exit 1
fi

if ! command -v dpkg >/dev/null 2>&1; then
	echo "Нет dpkg: это не Debian-контроллер Wiren Board." >&2
	exit 1
fi

arch=$(dpkg --print-architecture)

case "$arch" in
arm64) model="Wiren Board 8" ;;
armhf) model="Wiren Board 6/7" ;;
*)
	echo "Архитектура $arch не поддерживается: нужны arm64 (WB8) или armhf (WB6/WB7)." >&2
	exit 1
	;;
esac

if [ "$version" = "latest" ]; then
	url="https://github.com/$repo/releases/latest/download/wb-telegram_$arch.deb"
else
	url="https://github.com/$repo/releases/download/$version/wb-telegram_$arch.deb"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Контроллер: $model ($arch). Скачиваю $url"

if command -v curl >/dev/null 2>&1; then
	curl -fL --retry 3 -o "$tmp/wb-telegram.deb" "$url"
else
	wget -q -O "$tmp/wb-telegram.deb" "$url"
fi

# Настройки из формы (/etc/wb-telegram.conf) сохраняются без вопросов dpkg:
# через `curl | sh` ответить на них нельзя, а новые поля бот подставляет сам.
dpkg -i --force-confdef --force-confold "$tmp/wb-telegram.deb"

echo
echo "Установлено: $(dpkg-query -W -f '${Version}' wb-telegram)."
echo "Дальше: веб-интерфейс контроллера → Настройки → Конфигурационные файлы → Telegram-бот."
