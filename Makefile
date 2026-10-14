.PHONY: all go test clean

all: bin/rs-capture go

bin/rs-capture: native/capture/rs-capture.c
	@mkdir -p bin
	clang -O2 -Wall -Wextra -std=c11 -framework CoreAudio -framework CoreFoundation -o $@ $<

go:
	go build -o bin/ ./cmd/...

test:
	go vet ./...
	go test ./...

clean:
	rm -rf bin
