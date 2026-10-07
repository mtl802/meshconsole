BINS := bin/meshconsole bin/meshagent
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
CROSS_TARGETS := linux/amd64 darwin/arm64 windows/amd64

GO ?= go

# console 配置文件路径（pki 目标读取其中的 pki_dir/tailnet_ip）。
CONFIG ?= console.yaml

.PHONY: build cross test vet fmt clean pki

build: $(BINS)

bin/meshconsole: $(shell find cmd internal -name '*.go')
	$(GO) build -ldflags '$(LDFLAGS)' -o $@ ./cmd/console

bin/meshagent: $(shell find cmd internal -name '*.go')
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
