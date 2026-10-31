# ai-trace 构建脚本（零第三方依赖，离线可构建）
BINARY := ai-trace
VERSION ?= 0.1.0

.PHONY: build test release product clean

# 本机构建
build:
	go build -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/ai-trace

# 运行全部测试
test:
	go test ./...

# 三平台交叉编译（纯标准库，无需 CGO）
release:
	GOOS=darwin  GOARCH=amd64 go build -ldflags "-s -w -X main.version=$(VERSION)" -o dist/$(BINARY)-darwin-amd64 ./cmd/ai-trace
	GOOS=darwin  GOARCH=arm64 go build -ldflags "-s -w -X main.version=$(VERSION)" -o dist/$(BINARY)-darwin-arm64 ./cmd/ai-trace
	GOOS=linux   GOARCH=amd64 go build -ldflags "-s -w -X main.version=$(VERSION)" -o dist/$(BINARY)-linux-amd64 ./cmd/ai-trace
	GOOS=linux   GOARCH=arm64 go build -ldflags "-s -w -X main.version=$(VERSION)" -o dist/$(BINARY)-linux-arm64 ./cmd/ai-trace
	GOOS=windows GOARCH=amd64 go build -ldflags "-s -w -X main.version=$(VERSION)" -o dist/$(BINARY)-windows-amd64.exe ./cmd/ai-trace
	ls -lh dist/

# 成品打包：macOS DMG + Windows zip → release/v$(VERSION)/
product:
	scripts/release.sh $(VERSION)

clean:
	rm -rf dist $(BINARY) release
