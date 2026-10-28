.PHONY: all go native test clean relay-linux

JUCE_DIR ?=
# Relay built into rsession, e.g. make RELAY=relay.example.com:7401
RELAY ?=
CMAKE_JUCE := $(if $(JUCE_DIR),-DFETCHCONTENT_SOURCE_DIR_JUCE=$(JUCE_DIR),)

all: bin/rs-capture go native

bin/rs-capture: native/capture/rs-capture.c
	@mkdir -p bin
	clang -O2 -Wall -Wextra -std=c11 -framework CoreAudio -framework CoreFoundation -o $@ $<

go:
	go build -ldflags "-X main.defaultRelay=$(RELAY)" -o bin/ ./cmd/...

# Relay server for a Linux host (amd64; set GOARCH=arm64 for ARM servers).
relay-linux:
	GOOS=linux GOARCH=$${GOARCH:-amd64} CGO_ENABLED=0 go build -o bin/linux/rs-relay ./cmd/rs-relay

# JUCE parts: rs-render (plugin host) and the RemoteSession Link plugin.
native:
	cmake -S native -B native/build -DCMAKE_BUILD_TYPE=Release $(CMAKE_JUCE)
	cmake --build native/build --config Release -j8
	@mkdir -p bin
	cp native/build/rs-render_artefacts/Release/rs-render bin/

test:
	go vet ./...
	go test ./...

clean:
	rm -rf bin native/build
