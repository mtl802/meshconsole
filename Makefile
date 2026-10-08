BINS := bin/meshconsole bin/meshagent
# R19-#5：版本语义化为批次号（M1B_B）+ git 短哈希后缀，经 ldflags 注入两二进制；
# `meshconsole --version` / `meshagent --version` 输出版本形如 m1b-b+git_<short_hash>。
M1B_B := m1b-b
COMMIT ?= $(shell git rev-parse --short=8 HEAD 2>/dev/null || echo none)
VERSION ?= $(M1B_B)+git_$(COMMIT)
# 观察点②：版本与 commit 双双注入两二进制（-X main.version / -X main.commit），
# 与启动日志可核验。
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
CROSS_TARGETS := linux/amd64 darwin/arm64 windows/amd64

GO ?= go

# console 配置文件路径（pki 目标读取其中的 pki_dir/tailnet_ip）。
CONFIG ?= console.yaml

# 面板静态资源（embed 进二进制）也纳入重建依赖：改 web/* 不重跑 build 会打出旧面板。
WEB_ASSETS := $(shell find internal/panel/web -type f 2>/dev/null)

# R21-#4：构建配方（Makefile）与依赖清单（go.mod/go.sum）变更触发 bin 重建，
# 已有产物不再携带旧依赖/旧构建配方；commit 哈希变化不进依赖——不因提交触发
# 全量重建，交付时显式 make cross 保证版本新鲜度。
BUILD_DEPS := Makefile go.mod go.sum

.PHONY: build cross test vet fmt clean pki

build: $(BINS)

bin/meshconsole: $(shell find cmd internal -name '*.go') $(WEB_ASSETS) $(BUILD_DEPS)
	$(GO) build -ldflags '$(LDFLAGS)' -o $@ ./cmd/console

bin/meshagent: $(shell find cmd internal -name '*.go') $(BUILD_DEPS)
	$(GO) build -ldflags '$(LDFLAGS)' -o $@ ./cmd/agent

# pki：幂等生成 CA + 服务端证书到 pki_dir（缺省 ./pki，私钥 0600）。
# 已存在则不重新生成、不覆盖私钥；打印服务端证书 SHA-256 指纹供 agent 配置。
# 注意勿并行运行（无跨进程锁，R11-E）：并发执行可能产生配对不一致产物。
pki: bin/meshconsole
	./bin/meshconsole pki -config $(CONFIG)

# 三平台交叉编译（SPEC 验收 1）：linux/amd64、darwin/arm64、windows/amd64
cross:
	set -e; for t in $(CROSS_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		ext=; [ "$$os" = windows ] && ext=.exe; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			$(GO) build -trimpath -ldflags '$(LDFLAGS)' \
			-o bin/meshconsole-$$os-$$arch$$ext ./cmd/console; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			$(GO) build -trimpath -ldflags '$(LDFLAGS)' \
			-o bin/meshagent-$$os-$$arch$$ext ./cmd/agent; \
		echo "built bin/meshconsole-$$os-$$arch$$ext bin/meshagent-$$os-$$arch$$ext"; \
	done

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

clean:
	rm -rf bin
