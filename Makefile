.PHONY: all test clean

all: bin/rs-capture bin/rsession bin/loopbacktest

bin/rs-capture: native/capture/rs-capture.c
	@mkdir -p bin
	clang -O2 -Wall -Wextra -std=c11 -framework CoreAudio -framework CoreFoundation -o $@ $<

bin/rsession bin/loopbacktest: $(shell find cmd internal -name '*.go') go.mod
	go build -o bin/ ./cmd/...

test:
	go vet ./...
	go test ./...

clean:
	rm -rf bin
