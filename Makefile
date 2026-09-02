BIN := bin/splitr
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/tasticolly/splitr/internal/daemon.Version=$(VERSION)

.PHONY: help all build test test-race test-pf docker-test menubar fmt lint \
        install update uninstall doctor logs version release rollback clean

# help первым: `make` без аргументов показывает, что вообще можно.
help:
	@echo "SplitR $(VERSION)"
	@echo
	@echo "  make update        собрать, проверить и переустановить всё (нужен sudo)"
	@echo "  make doctor        проверить установку"
	@echo "  make version       текущая версия"
	@echo
	@echo "  make test          юнит-тесты"
	@echo "  make docker-test   e2e с настоящим sshd и sshuttle"
	@echo "  make test-pf       нативный тест pf (нужен sudo, туннель должен быть опущен)"
	@echo
	@echo "  make release V=v0.3.0 M='что изменилось'   выпустить версию"
	@echo "  make rollback V=v0.2.0                     откатиться на версию"
	@echo
	@echo "  make uninstall     снять всё"

all: build test

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/splitr

# Юнит-тесты: без root, без сети, без изменения системы.
test:
	go test ./...

test-race:
	go test -race ./...

# Нативный e2e поверх настоящего pf — единственное, что проверяет решения ядра.
test-pf:
	sudo ./test/pfe2e/run.sh

# Полный e2e в Docker: настоящий sshd, настоящий sshuttle, подменённый pfctl.
docker-test:
	./test/docker/run.sh

fmt:
	gofmt -l -w .

lint:
	@gofmt -l . | tee /tmp/splitr-fmt.txt
	@[ ! -s /tmp/splitr-fmt.txt ] || { echo "не отформатировано (см. выше)"; exit 1; }
	go vet ./...

menubar:
	./menubar/build.sh

# Обновление одной командой: и демон, и приложение в строке меню.
# Отдельный install оставлен для первой установки, но в быту нужен именно update.
update: lint test build menubar
	sudo $(BIN) install
	./menubar/install.sh
	@echo
	@$(BIN) doctor

install: build
	sudo $(BIN) install

uninstall:
	sudo $(BIN) uninstall
	-./menubar/uninstall.sh

doctor: build
	$(BIN) doctor

version:
	@echo "исходники:   $(VERSION)"
	@echo "установлено: $$(/usr/local/bin/splitr --version 2>/dev/null || echo не установлено)"

logs:
	$(BIN) log --tail 200

release:
	@[ -n "$(V)" ] || { echo "укажи версию: make release V=v0.3.0 M='описание'"; exit 1; }
	./scripts/release.sh "$(V)" "$(M)"

rollback:
	@[ -n "$(V)" ] || { echo "укажи версию: make rollback V=v0.2.0"; exit 1; }
	./scripts/rollback.sh "$(V)"

clean:
	rm -rf bin menubar/build
