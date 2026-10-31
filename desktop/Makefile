# Oscillo 桌面客户端构建（macOS .app 组装，无第三方工具链依赖）
# 用法：make app [VERSION=0.1.0]
VERSION ?= 0.1.0
APP := Oscillo

# 成品打包（macOS DMG + Windows zip），产物在 ../release/
release:
	../scripts/release.sh $(VERSION)

app: build-bin
	@mkdir -p "$(APP).app/Contents/MacOS" "$(APP).app/Contents/Resources"
	@cp ai-trace "$(APP).app/Contents/MacOS/ai-trace"
	@cp build/Oscillo.icns "$(APP).app/Contents/Resources/Oscillo.icns"
	@sed 's/@VERSION@/$(VERSION)/' build/Info.plist > "$(APP).app/Contents/Info.plist"
	@echo "已组装 $(APP).app（$(VERSION)）"

build-bin:
	go build -ldflags "-X main.version=$(VERSION)" -o ai-trace .

clean:
	rm -rf ai-trace $(APP).app ai-trace.app

.PHONY: app build-bin release clean
