BINS := bin/meshconsole bin/meshagent
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
CROSS_TARGETS := linux/amd64 darwin/arm64 windows/amd64

GO ?= go

.PHONY: build cross test vet fmt clean

build: $(BINS)

bin/meshconsole: $(shell find cmd internal -name '*.go')
	$(GO) build -ldflags '$(LDFLAGS)' -o $@ ./cmd/console

bin/meshagent: $(shell find cmd internal -name '*.go')
	$(GO) build -ldflags '$(LDFLAGS)' -o $@ ./cmd/agent

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
