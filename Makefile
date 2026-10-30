# whatsapp-mcp — build, check, install, and run as a systemd user service.
#
#   make                  build bin/whatsapp-mcp
#   make check            gofmt + vet + tests
#   make install          copy the binary to $(BINDIR)
#   make service          install + write and start a systemd user unit (HTTP daemon)
#   make uninstall        stop/remove the unit and the installed binary (the archive is kept)

BINARY  := whatsapp-mcp
PREFIX  ?= $(HOME)/.local
BINDIR  ?= $(PREFIX)/bin
UNITDIR ?= $(HOME)/.config/systemd/user
ADDR    ?= 127.0.0.1:8765

# Version: the last commit touching this directory, or -dev while it's untracked.
GIT_REV := $(shell git log -1 --format=%h -- . 2>/dev/null)
VERSION ?= 0.1.0$(if $(GIT_REV),+$(GIT_REV),-dev)

# go-sqlite3 is a cgo package: cgo must be on and a C compiler present.
export CGO_ENABLED := 1
GOFLAGS_BUILD := -trimpath -ldflags "-s -w -X main.version=$(VERSION)"
SOURCES := $(wildcard *.go) go.mod go.sum

.PHONY: all build check fmt vet test install service service-local service-start uninstall clean

all: build

build: bin/$(BINARY)

bin/$(BINARY): $(SOURCES)
	go build $(GOFLAGS_BUILD) -o $@ .

check: vet test
	@unformatted=$$(gofmt -l .); if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on: $$unformatted (run make fmt)"; exit 1; fi

fmt:
	gofmt -w .

vet:
	go vet ./...

test:
	go test -count=1 ./...

install: build
	install -d $(BINDIR)
	install -m 0755 bin/$(BINARY) $(BINDIR)/$(BINARY)
	@echo "installed $(BINDIR)/$(BINARY) ($(VERSION))"

# Runs the connection-owning daemon so the archive keeps filling while no MCP client is open.
# Link the account first (`$(BINARY) login`). Point clients at http://$(ADDR)/mcp.
service: install
	$(MAKE) service-start

# Development: run this checkout's binary, without copying it to ~/.local/bin.
service-local: build
	$(MAKE) service-start BINDIR="$(CURDIR)/bin"

service-start:
	install -d $(UNITDIR)
	printf '%s\n' \
		'[Unit]' \
		'Description=WhatsApp MCP server (read-only archive of your WhatsApp account)' \
		'After=network-online.target' \
		'Wants=network-online.target' \
		'' \
		'[Service]' \
		'ExecStart=$(BINDIR)/$(BINARY) serve --transport http --addr $(ADDR)' \
		'Restart=on-failure' \
		'RestartSec=10' \
		'' \
		'[Install]' \
		'WantedBy=default.target' > $(UNITDIR)/$(BINARY).service
	systemctl --user daemon-reload
	systemctl --user enable --now $(BINARY).service
	systemctl --user restart $(BINARY).service
	@echo "service running: http://$(ADDR)/mcp  (logs: journalctl --user -u $(BINARY) -f)"
	@loginctl show-user "$$USER" -p Linger 2>/dev/null | grep -q yes || \
		echo "note: user services stop when you log out; run 'loginctl enable-linger $$USER' to keep it running"

uninstall:
	-systemctl --user disable --now $(BINARY).service 2>/dev/null
	rm -f $(UNITDIR)/$(BINARY).service $(BINDIR)/$(BINARY)
	-systemctl --user daemon-reload
	@echo "removed; the archive in ~/.local/share/whatsapp-mcp was kept"

clean:
	rm -rf bin
