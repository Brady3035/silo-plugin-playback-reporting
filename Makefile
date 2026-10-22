# Version of a local build. Releases take theirs from the git tag (vX.Y.Z).
VERSION ?= 0.0.0-dev
REPO ?= Brady3035/silo-plugin-playback-reporting
LDFLAGS = -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/arm64

.PHONY: build check dist index clean

go.sum: go.mod
	go mod tidy

build: go.sum
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o plugin .

check: go.sum
	test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...
	go test -race ./...

# Release binaries named the way Silo's catalog expects (plugin-<os>-<arch>)
# plus checksums.txt.
dist: go.sum
	rm -rf release && mkdir -p release
	for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o release/plugin-$$os-$$arch . || exit 1; \
	done
	cd release && sha256sum plugin-* > checksums.txt

# The repository index Silo reads (needs `make dist` first).
index:
	go run ./cmd/repoindex -version $(VERSION) -repo $(REPO) -dist release -out repository.json

clean:
	rm -rf release plugin
