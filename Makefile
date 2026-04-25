.PHONY: build install-user test dev

BIN_DIR := $(CURDIR)/bin
USER_BIN_DIR := $(HOME)/.local/bin

build:
	@mkdir -p "$(BIN_DIR)"
	go build -o "$(BIN_DIR)/otter" ./cmd/otter

install-user: build
	@mkdir -p "$(USER_BIN_DIR)"
	@ln -sf "$(BIN_DIR)/otter" "$(USER_BIN_DIR)/otter"
	@echo "Installed: $(USER_BIN_DIR)/otter"
	@echo "If needed, add to PATH:"
	@echo "  export PATH=\"$(USER_BIN_DIR):\$$PATH\""

test:
	go test ./...

dev: build
	@if [ -z "$$OTTER_TOKEN" ]; then echo "OTTER_TOKEN is required"; exit 1; fi
	"$(BIN_DIR)/otter" serve
