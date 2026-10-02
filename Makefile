###### ПЕРЕМЕННЫЕ ######
# Имя бинаря, пакета и сервиса.
BINARY := wb-telegram
# Каталог локальных инструментов (golangci-lint, mockgen).
LOCAL_BIN := $(CURDIR)/bin
# Префикс установки, как у пакетов Wiren Board.
PREFIX ?= /usr
# Архитектура бинаря: arm64 — WB8, armhf — WB6/WB7; при сборке пакета её задаёт dpkg-buildpackage.
DEB_TARGET_ARCH ?= arm64
# Версия, зашиваемая в бинарь.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

GO ?= go
GO_LDFLAGS := -s -w -X main.version=$(VERSION)
GO_FLAGS := -buildvcs=false -trimpath -ldflags="$(GO_LDFLAGS)"
GOARCH_arm64 := GOARCH=arm64
GOARCH_armhf := GOARCH=arm GOARM=6
GOARCH_amd64 := GOARCH=amd64
GO_ENV := GOOS=linux CGO_ENABLED=0 $(GOARCH_$(DEB_TARGET_ARCH))

# Цель по умолчанию: dh_auto_build вызывает голый make.
.DEFAULT_GOAL := build
###### ПЕРЕМЕННЫЕ ######

###### СБОРКА ######
# Статический бинарь под контроллер.
.PHONY: build
build:
	$(GO_ENV) $(GO) build $(GO_FLAGS) -o $(BINARY) ./cmd

# Удалить собранный бинарь и пакеты.
.PHONY: clean
clean:
	rm -rf $(BINARY) dist
###### СБОРКА ######

###### ТЕСТЫ ######
.PHONY: test
test:
	$(GO) test -race ./... -count=1 -timeout=60s -v -short
###### ТЕСТЫ ######

###### ЛИНТ ######
# Lint только изменений относительно origin/master (ветки и PR).
.PHONY: lint
lint: install-lint
	$(LOCAL_BIN)/golangci-lint run --new-from-rev=origin/master --config=.golangci.yml ./...

# Lint всего кода.
.PHONY: lint-full
lint-full: install-lint
	$(LOCAL_BIN)/golangci-lint run --config=.golangci.yml ./...
###### ЛИНТ ######

###### ИНСТРУМЕНТЫ ######
LINT_VERSION ?= v2.14.0
MOCKGEN_VERSION ?= v0.6.0

# install_tool ставит $(2)@$(3) в ./bin под именем $(1), если там нет маркера bin/.versions/$(1)/$(3).
define install_tool
	@if [ -x "$(LOCAL_BIN)/$(1)" ] && [ -f "$(LOCAL_BIN)/.versions/$(1)/$(3)" ]; then \
		echo "$(1) $(3) is already installed"; \
	else \
		echo "Installing $(1) $(3)"; \
		GOBIN=$(LOCAL_BIN) $(GO) install $(2)@$(3) && \
		rm -rf "$(LOCAL_BIN)/.versions/$(1)" && \
		mkdir -p "$(LOCAL_BIN)/.versions/$(1)" && \
		touch "$(LOCAL_BIN)/.versions/$(1)/$(3)"; \
	fi
endef

.PHONY: install-lint
install-lint:
	$(call install_tool,golangci-lint,github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(LINT_VERSION))

.PHONY: install-mockgen
install-mockgen:
	$(call install_tool,mockgen,go.uber.org/mock/mockgen,$(MOCKGEN_VERSION))

# Перегенерировать моки (go:generate).
.PHONY: generate
generate: install-mockgen
	PATH=$(LOCAL_BIN):$$PATH $(GO) generate ./...

.PHONY: tidy
tidy:
	$(GO) mod tidy
###### ИНСТРУМЕНТЫ ######

###### ПАКЕТ ######
# Раскладка файлов пакета; вызывается dh_auto_install с DESTDIR.
.PHONY: install
install: build
	install -Dm0755 $(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)
	install -Dm0644 wb/confed/wb-telegram.schema.json $(DESTDIR)$(PREFIX)/share/wb-mqtt-confed/schemas/wb-telegram.schema.json
	install -Dm0644 wb/rules-modules/telegram.js $(DESTDIR)$(PREFIX)/share/wb-rules-modules/telegram.js
	install -Dm0644 wb/rules-examples/telegram-example.js $(DESTDIR)$(PREFIX)/share/doc/wb-telegram/examples/telegram-example.js
	install -Dm0644 wb/default-config/wb-telegram.conf $(DESTDIR)/etc/wb-telegram.conf
	install -d -m0700 $(DESTDIR)/etc/wb-telegram

# Образ для сборки пакета в Docker (в том числе на macOS с Apple Silicon).
DEB_IMAGE ?= golang:1.26-trixie
# Архитектура пакета: arm64 — WB8, armhf — WB6/WB7.
ARCH ?= arm64
# Версия пакета; пусто — из debian/changelog.
DEB_VERSION ?=

# Пакет в контейнере; результат — dist/wb-telegram_<версия>_<ARCH>.deb.
.PHONY: deb
deb:
	docker run --rm -v "$(CURDIR):/src" -w /src $(DEB_IMAGE) sh -c '\
		apt-get update -qq && apt-get install -y -qq --no-install-recommends debhelper dpkg-dev \
			binutils-aarch64-linux-gnu binutils-arm-linux-gnueabihf >/dev/null && \
		git config --global --add safe.directory /src && \
		scripts/build-deb.sh $(ARCH) $(DEB_VERSION) && chown -R $(shell id -u):$(shell id -g) dist'
###### ПАКЕТ ######

###### ВЫКЛАДКА ######
# Адрес контроллера, например root@192.168.1.10 (в репозиторий не записывается).
CONTROLLER ?=
# Ключ SSH.
SSH_KEY ?= $(HOME)/.ssh/id_ed25519

# Быстрая выкладка бинаря поверх установленного пакета (для разработки).
.PHONY: deploy
deploy: build
	@test -n "$(CONTROLLER)" || { echo "usage: make deploy CONTROLLER=root@<ip> [SSH_KEY=...]"; exit 1; }
	scp -i $(SSH_KEY) $(BINARY) $(CONTROLLER):/usr/bin/$(BINARY).new
	ssh -i $(SSH_KEY) $(CONTROLLER) 'mv /usr/bin/$(BINARY).new /usr/bin/$(BINARY) && systemctl restart $(BINARY) && systemctl --no-pager status $(BINARY) | head -5'
###### ВЫКЛАДКА ######
