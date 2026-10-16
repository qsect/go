.PHONY: all tidy build build-linux run clean

BINARY_NAME=vite-server
BUILD_DIR=./bin

all: build

tidy:
	GOPROXY=https://goproxy.cn,direct go mod tidy

build: tidy
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/server

build-linux: tidy
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/server

run: build
	$(BUILD_DIR)/$(BINARY_NAME)

clean:
	rm -rf $(BUILD_DIR)
