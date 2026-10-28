# AntiMusic RemoteSession

Remote recording for DAW sessions. This repository currently holds the
recording foundation:

- a lossless local master that does not depend on the network;
- a journal of DAW transport;
- Broadcast Wave deliverables that land at the right place on the Pro Tools
  timeline;
- offline post-processing prints through AU/VST3 plugin chains (`rs-render`);
- the RemoteSession Link plugin (AAX/AU/VST3), which stamps track audio with
  the host's exact timeline position.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for decisions, measurements
and open blockers.

## Build and test

Needs macOS with Xcode command line tools, Go 1.25+, CMake 3.22+ and FFmpeg.
The native build downloads JUCE 8.0.10 on first run; pass
`JUCE_DIR=/path/to/JUCE` to use an existing checkout.

```sh
make          # bin/rs-capture (C, CoreAudio), Go tools, bin/rs-render and the Link plugin
make go       # Go tools only: bin/rsession, bin/loopbacktest, bin/ptslverify
make test     # unit and end-to-end tests (needs ffmpeg; PTSL schema tests need Pro Tools installed)
```

Plugin builds land in `native/build/RemoteSessionLink_artefacts/Release/`.
The AAX build is unsigned (see the blockers in the architecture doc).

## Use

```sh
bin/rsession devices                                   # CoreAudio inputs (name, UID, channels, rate)

# Record against the open Pro Tools session (transport journaled via PTSL):
bin/rsession record -device "Pro Tools Audio Bridge 16" -dir takes/take1 -ptsl
# ...or with session details given manually:
bin/rsession record -device "MacBook Pro Microphone" -dir takes/take2 -session-rate 48000 -session-start 01:00:00:00 -fps 25

bin/rsession deliver -dir takes/take1                  # BWF per transport run, verified sample by sample
bin/rsession ptsl-import -dir takes/take1              # import into Pro Tools at each file's position
bin/rsession ptsl-status

# Exact transport from the Link plugin instead of PTSL polling:
bin/rsession links                                     # running Link instances
bin/rsession record -link "Vox" -dir takes/take3

# Remote: record at home, land the take on the studio machine.
# Studio (receives, delivers, imports into the open Pro Tools session):
RS_TOKEN=<secret> bin/rsession receive -dir takes/received -import
# Home (records locally and copies live; regular Pro Tools via Audio Bridge + PTSL):
RS_TOKEN=<secret> bin/rsession record -device "Pro Tools Audio Bridge 2-A" -dir takes/home1 -ptsl -send <studio host>
# If the link drops, recording carries on; resume or re-send any take with:
RS_TOKEN=<secret> bin/rsession send -dir takes/home1 -to <studio host>

# Post-processing print through a plugin chain:
bin/rsession deliver -dir takes/take1 -chain chain.json -label limited -tail 4800
# chain.json: [{"id":"<AU id or plugin path>","state":"<optional state file>"}]
```

Hardware and DAW verification tools:

```sh
bin/loopbacktest -device "Pro Tools Audio Bridge 16" -rate 48000 -channels 16 -seconds 60
bin/ptslverify -dir /tmp/rs-verify    # needs Pro Tools running; makes and discards its own session
```

## Layout

| Path | Contents |
|---|---|
| `native/capture/` | `rs-capture`, real-time-safe CoreAudio capture |
| `native/render/` | `rs-render`, offline AU/VST3 chain renderer (JUCE) |
| `native/link/` | RemoteSession Link plugin and its shared-memory ring format |
| `internal/link` | reader for the Link plugin's ring |
| `internal/recorder` | starts the capture helper and FFmpeg, writes the take journal |
| `internal/take` | take directory format |
| `internal/timeline` | transport events → placed segments, timecode, capture clock |
| `internal/deliver` | BWF rendering and independent verification |
| `internal/ptsl` | Pro Tools Scripting Library client |
| `internal/transport` | PTSL transport watcher |
| `internal/wav`, `internal/caf`, `internal/pcm` | file readers used for verification |
