# AntiMusic RemoteSession

Remote recording for DAW sessions. This repository currently holds the
recording foundation:

- a lossless local master that does not depend on the network;
- a journal of DAW transport;
- Broadcast Wave deliverables that land at the right place on the Pro Tools
  timeline.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for decisions, measurements
and open blockers.

## Build and test

```sh
make          # bin/rs-capture (C, CoreAudio), bin/rsession, bin/loopbacktest, bin/ptslverify
make test     # unit and end-to-end tests (needs ffmpeg; PTSL schema tests need Pro Tools installed)
```

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
| `internal/recorder` | starts the capture helper and FFmpeg, writes the take journal |
| `internal/take` | take directory format |
| `internal/timeline` | transport events → placed segments, timecode, capture clock |
| `internal/deliver` | BWF rendering and independent verification |
| `internal/ptsl` | Pro Tools Scripting Library client |
| `internal/transport` | PTSL transport watcher |
| `internal/wav`, `internal/caf`, `internal/pcm` | file readers used for verification |
