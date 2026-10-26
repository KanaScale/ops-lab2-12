# 实验 2 的构建入口。需要 Go 1.26 或更高版本、GNU make；没有第三方依赖，构建时不用联网。
#
#   make build        编译 bin/enrolld、bin/loadgen、bin/promsteps
#   make test         运行测试
#   make fmt-check vet

BIN := bin
CMDS := enrolld loadgen promsteps

.PHONY: build test vet fmt-check clean

build:
	@mkdir -p $(BIN)
	@for c in $(CMDS); do echo "go build -o $(BIN)/$$c ./cmd/$$c"; CGO_ENABLED=0 go build -trimpath -o $(BIN)/$$c ./cmd/$$c || exit 1; done

test:
	go test -count=1 ./...

vet:
	go vet ./...

fmt-check:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "以下文件需要 gofmt："; echo "$$out"; exit 1; fi

clean:
	rm -rf $(BIN)
