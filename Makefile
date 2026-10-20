VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     := bin

.PHONY: all tidy build test vet operator operator-test operator-image install clean

all: vet test build operator

tidy:
	go mod tidy
	cd operator && go mod tidy

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/horus ./cmd/horus

test:
	go test -race ./...

vet:
	go vet ./...
	cd operator && go vet ./...

operator:
	cd operator && CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o ../$(BIN)/horus-operator ./cmd

operator-test:
	cd operator && go test -race ./...

operator-image:
	docker build -f operator/Dockerfile --build-arg VERSION=$(VERSION) -t horus-operator:$(VERSION) .

# Installs the server on this machine (run as root). Creates the admin token
# only if it does not exist yet.
install: build
	install -m 0755 $(BIN)/horus /usr/local/bin/horus
	install -d -m 0755 /etc/horus
	test -f /etc/horus/horus.env || install -m 0644 deploy/systemd/horus.env.example /etc/horus/horus.env
	test -f /etc/horus/admin.token || (umask 077 && /usr/local/bin/horus token > /etc/horus/admin.token)
	install -m 0644 deploy/systemd/horus.service /etc/systemd/system/horus.service
	install -m 0644 deploy/systemd/horus-backup.service /etc/systemd/system/horus-backup.service
	install -m 0644 deploy/systemd/horus-backup.timer /etc/systemd/system/horus-backup.timer
	systemctl daemon-reload
	@echo "Now: systemctl enable --now horus horus-backup.timer && horus zone ls"

clean:
	rm -rf $(BIN)
