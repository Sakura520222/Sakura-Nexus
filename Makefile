# Sakura-Bot v2 —— 构建/测试/检查（Go 部分；前端 job 由 T5.4 追加）

BINARY := sakura-bot

.PHONY: build test test-integration lint fmt fmt-check tidy

build:
	go build -o $(BINARY) ./cmd/sakura-bot

test:
	go test -race ./...

# 本地集成环境：docker compose -f compose.test.yaml up -d（MySQL，T0.2 交付）
test-integration:
	go test -race -tags integration ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt 需处理:"; echo "$$out"; exit 1; fi

tidy:
	go mod tidy
