# Sakura-Nexus —— 构建/测试/检查（Go 部分；前端 job 由 T5.4 追加）

BINARY := sakura-nexus

.PHONY: build test test-local test-integration lint fmt fmt-check tidy

build:
	go build -o $(BINARY) ./cmd/sakura-nexus

test:
	go test -race ./...

# 本地无 cgo/gcc 环境（如 Windows 无 mingw）时的开发验证；门禁以 CI 的 -race 为准
test-local:
	go test ./...

# 本地集成环境：SAKURA_TEST_MYSQL_* 指向自备 MySQL（可 export .env.test.local 中的值；未设置则 skip）
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
