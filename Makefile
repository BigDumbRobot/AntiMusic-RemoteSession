.PHONY: all go native test clean relay-linux app icons

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

# RemoteSession.app: the window app with rsession and helpers inside.
# Universal (Apple Silicon + Intel); ad-hoc signed.
APP := build/RemoteSession.app
app: native
	rm -rf $(APP) build/app-tmp && mkdir -p $(APP)/Contents/MacOS $(APP)/Contents/Resources/bin build/app-tmp
	swiftc -parse-as-library -O -target arm64-apple-macos13 -o build/app-tmp/ui-arm64 app/RemoteSession/main.swift
	swiftc -parse-as-library -O -target x86_64-apple-macos13 -o build/app-tmp/ui-x86_64 app/RemoteSession/main.swift
	lipo -create -output $(APP)/Contents/MacOS/RemoteSession build/app-tmp/ui-arm64 build/app-tmp/ui-x86_64
	GOARCH=arm64 go build -ldflags "-X main.defaultRelay=$(RELAY)" -o build/app-tmp/rsession-arm64 ./cmd/rsession
	GOARCH=amd64 go build -ldflags "-X main.defaultRelay=$(RELAY)" -o build/app-tmp/rsession-amd64 ./cmd/rsession
	lipo -create -output $(APP)/Contents/Resources/bin/rsession build/app-tmp/rsession-arm64 build/app-tmp/rsession-amd64
	clang -O2 -Wall -Wextra -std=c11 -arch arm64 -arch x86_64 -mmacosx-version-min=11.0 -framework CoreAudio -framework CoreFoundation \
		-o $(APP)/Contents/Resources/bin/rs-capture native/capture/rs-capture.c
	cp native/build/rs-render_artefacts/Release/rs-render $(APP)/Contents/Resources/bin/
	cp app/RemoteSession/Info.plist $(APP)/Contents/
	mkdir -p build/app-tmp/AppIcon.iconset
	for s in 16 32 128 256 512; do \
		sips -z $$s $$s assets/icons/app-icon.png --out build/app-tmp/AppIcon.iconset/icon_$${s}x$${s}.png >/dev/null; \
		sips -z $$((s*2)) $$((s*2)) assets/icons/app-icon.png --out build/app-tmp/AppIcon.iconset/icon_$${s}x$${s}@2x.png >/dev/null; \
	done
	iconutil -c icns -o $(APP)/Contents/Resources/AppIcon.icns build/app-tmp/AppIcon.iconset
	codesign --force --deep --sign - $(APP)
	rm -rf build/app-tmp
	@echo "built $(APP)"

# Regenerate assets/icons from the logo (tools/icons/make_icons.swift).
icons:
	@mkdir -p build
	swiftc -O -o build/make_icons tools/icons/make_icons.swift
	build/make_icons .
