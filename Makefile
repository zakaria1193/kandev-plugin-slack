.PHONY: build run test fmt vet package package-host verify-package verify-package-host clean

BIN := bin/kandev-plugin-slack
VERSION := 0.2.0-fork.1
STAGE := .build/stage
PKG_OUT := kandev-plugin-slack-$(VERSION).tar.gz
KANDEV_BACKEND := ../kandev/apps/backend

## Build the plugin binary for the host platform. Development use only —
## the installed-plugin path always goes through package/package-host.
build:
	mkdir -p bin
	go build -o $(BIN) ./server

## Build + run. Mainly a smoke check: kandev normally spawns this binary
## itself over the go-plugin handshake, so a bare run just blocks.
run: build
	./$(BIN)

test:
	go test ./server

fmt:
	gofmt -l .

vet:
	go vet ./server

## Cross-compile every platform declared in manifest.yaml's
## runtime.executables, stage manifest.yaml + assets/ + ui/ alongside them,
## and pack the tree with Kandev's cmd/plugin-pack from the sibling backend.
package:
	rm -rf $(STAGE)
	mkdir -p $(STAGE)/server $(STAGE)/ui
	cp manifest.yaml $(STAGE)/manifest.yaml
	cp -r assets $(STAGE)/assets
	cp README.md $(STAGE)/README.md
	cp -r docs $(STAGE)/docs
	cp slack-app-manifest.yaml $(STAGE)/slack-app-manifest.yaml
	cp ui/bundle.js $(STAGE)/ui/bundle.js
	GOOS=linux   GOARCH=amd64 go build -o $(STAGE)/server/plugin-linux-amd64       ./server
	GOOS=linux   GOARCH=arm64 go build -o $(STAGE)/server/plugin-linux-arm64       ./server
	GOOS=darwin  GOARCH=amd64 go build -o $(STAGE)/server/plugin-darwin-amd64      ./server
	GOOS=darwin  GOARCH=arm64 go build -o $(STAGE)/server/plugin-darwin-arm64      ./server
	GOOS=windows GOARCH=amd64 go build -o $(STAGE)/server/plugin-windows-amd64.exe ./server
	go -C $(KANDEV_BACKEND) run ./cmd/plugin-pack -dir $(abspath $(STAGE)) -out $(abspath $(PKG_OUT))
	rm -rf $(STAGE)
	@echo "Wrote $(PKG_OUT)"

## Host platform only — the fast local iteration loop.
package-host:
	rm -rf $(STAGE)
	mkdir -p $(STAGE)/server $(STAGE)/ui
	cp manifest.yaml $(STAGE)/manifest.yaml
	cp -r assets $(STAGE)/assets
	cp README.md $(STAGE)/README.md
	cp -r docs $(STAGE)/docs
	cp slack-app-manifest.yaml $(STAGE)/slack-app-manifest.yaml
	cp ui/bundle.js $(STAGE)/ui/bundle.js
	go build -o $(STAGE)/server/plugin-$$(go env GOOS)-$$(go env GOARCH)$$(go env GOEXE) ./server
	go -C $(KANDEV_BACKEND) run ./cmd/plugin-pack -dir $(abspath $(STAGE)) -out $(abspath $(PKG_OUT)) -platform-only
	rm -rf $(STAGE)
	@echo "Wrote $(PKG_OUT)"

## Verify the generated archive, including the manifest-declared marketplace
## icon and plugin-pack's checksums, before it reaches the host installer.
define verify_package_archive
set -eu; \
VERIFY_DIR="$$(mktemp -d)"; \
trap 'rm -rf "$$VERIFY_DIR"' EXIT; \
test -f "$(PKG_OUT)" || { echo "package not found: $(PKG_OUT)"; exit 1; }; \
tar -xzf "$(PKG_OUT)" -C "$$VERIFY_DIR"; \
test -f "$$VERIFY_DIR/manifest.yaml"; \
grep -Fx 'icon: "assets/icon.svg"' "$$VERIFY_DIR/manifest.yaml" >/dev/null; \
test -f "$$VERIFY_DIR/assets/icon.svg"; \
test -f "$$VERIFY_DIR/assets/NOTICE.md"; \
test -f "$$VERIFY_DIR/README.md"; \
test -f "$$VERIFY_DIR/docs/notifications.md"; \
test -f "$$VERIFY_DIR/docs/clarification-threads.md"; \
test -f "$$VERIFY_DIR/slack-app-manifest.yaml"; \
test -f "$$VERIFY_DIR/ui/bundle.js"; \
test -f "$$VERIFY_DIR/checksums.txt"; \
grep -Eq '^[0-9a-f]{64}  assets/icon\.svg$$' "$$VERIFY_DIR/checksums.txt"; \
if command -v sha256sum >/dev/null 2>&1; then \
	(cd "$$VERIFY_DIR" && sha256sum -c checksums.txt); \
else \
	(cd "$$VERIFY_DIR" && shasum -a 256 -c checksums.txt); \
fi; \
$(1)
endef

verify-package:
	@$(call verify_package_archive,for executable in server/plugin-linux-amd64 server/plugin-linux-arm64 server/plugin-darwin-amd64 server/plugin-darwin-arm64 server/plugin-windows-amd64.exe; do test -f "$$VERIFY_DIR/$$executable" || { echo "package missing $$executable"; exit 1; }; done)

verify-package-host:
	@$(call verify_package_archive,test -f "$$VERIFY_DIR/server/plugin-$$(go env GOOS)-$$(go env GOARCH)$$(go env GOEXE)" || { echo "package missing host executable"; exit 1; })

clean:
	rm -rf bin $(STAGE) kandev-plugin-slack-*.tar.gz
