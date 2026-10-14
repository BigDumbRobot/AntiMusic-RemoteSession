# Architecture and evidence

Status as of 2026-10-14. This file records what has been decided and why. Each
decision cites a measurement made on the development machine. Statements marked
**unverified** have not been measured yet.

## Environment (inspected)

| Item | Found |
|---|---|
| OS | macOS 26.1 (25B78), Apple Silicon (arm64) |
| DAW | Pro Tools 26.4.0 build 175 ("2026.4"), Intel binary running under Rosetta |
| DAW scripting | PTSL, gRPC on `localhost:31416`; the schema is embedded in `Pro Tools.app/Contents/Frameworks/libPTSL.dylib` (proto3, self-contained). The PTSL C++ SDK zip in Dropbox is a 0-byte online-only placeholder. |
| Plugin SDKs | No AAX SDK on disk. JUCE 6.0.8 exists inside `~/sonobus` (third-party project, not used). |
| FFmpeg | 9.0.1 (Homebrew): `avfoundation` input, `audiotoolbox` output, WAV muxer with `write_bext` and `rf64`, CAF muxer |
| Toolchain | Go 1.25, Apple clang 21, Swift, CMake, Python 3.14 |
| Audio devices | Pro Tools Audio Bridge 2-A/2-B/6/16/32/64, VB-Cable (8 ch, 44.1 kHz), built-in microphone and others |

The repository was empty, so there was no existing code to preserve.

## The pipeline as built

```
CoreAudio device ──IOProc──► rs-capture ──raw f32 pipe──► FFmpeg ──► master.caf   (lossless, crash-safe)
                     │                                                   ▲
                     └─ device sample clock + host time ──► capture.log  │
DAW transport (PTSL now; plugin later) ──► transport.jsonl ──────────────┤
                                                                         ▼
                              deliver: segments ─► FFmpeg atrim ─► BWF (bext TimeReference)
                                                     └─► independent verification (Go readers)
                              ptsl-import: Import + ML_Spot at the session offset
```

A take directory (`internal/take`) holds everything needed to place the master
later, with no dependency on the process, DAW or network that produced it.

## Decisions and the evidence for them

### 1. Device capture uses a native CoreAudio helper, not FFmpeg's avfoundation input

FFmpeg remains the recorder: it encodes and writes the master. Audio enters
FFmpeg from `rs-capture` (`native/capture/rs-capture.c`) through a pipe.

Evidence: a known signal was played into Pro Tools Audio Bridge 2-A (48 kHz,
2 ch) and captured back for 30 s.

- `ffmpeg -f avfoundation` with this project's options: 272 whole 512-frame
  buffers dropped (2.9 s of 30 s). All 2,541 retained blocks were bit-exact.
- Plain `ffmpeg -f avfoundation` with no extra options: at least 853 blocks
  dropped (about 30 %).
- `rs-capture` (HAL IOProc, lock-free SPSC ring, writer thread): bit-exact
  with zero gaps.
  - 60 s at 2 ch and 60 s at 16 ch: 5,625 blocks = 2,880,000 frames, all
    bit-exact.
  - 30 s at 64 ch: 2,813 blocks, all bit-exact.
  - Ring overflows: 0. Continuity breaks: 0.

  Reproduce with `bin/loopbacktest -device "Pro Tools Audio Bridge 16" -rate 48000 -channels 16 -seconds 60`.

`rs-capture` logs each IO cycle's device sample time (`mSampleTime`) alongside
its host time. If a cycle is ever lost, the sample-time jump is exact. The gap is
filled with silence so that master frames stay locked to the device clock, and
it is marked in the log. Host-time jitter is not used for this.

Loopback-rig findings (properties of the rig, not of the recorder):

- With Pro Tools not running, a second client on the same Audio Bridge (FFmpeg's
  AudioToolbox player) made the bridge's input clock skip cycles erratically.
  The verifier therefore plays from the capture IOProc itself (`--play`): one
  client, one clock.
- VB-Cable is not bit-transparent: best-fit gain 0.695 with 12 % residual
  energy. Do not use it for verification.

### 2. The master is CAF written by FFmpeg with per-packet flush

Evidence: FFmpeg was killed with SIGKILL mid-recording.

| Format | Result |
|---|---|
| CAF | Data size stays `-1` ("to end of file"), so the file is spec-valid and readable. |
| WAV | RIFF/data sizes are never finalized. FFmpeg tolerates this; strict readers do not. |
| W64 | Unreadable. |

Without `-flush_packets 1`, about 0.2 s sat in FFmpeg's I/O buffer and was lost.
With it, nothing measurable was lost.

Covered by `TestInterruptedTakeStillDelivers`: a killed recorder still delivers
sample-exact, correctly placed files from the frames on disk.

Not yet handled: FFmpeg does not fsync, so on power loss the OS page cache can
still lose recent audio.

### 3. Delivery is Broadcast Wave with `bext` TimeReference; placement verified in Pro Tools

Each contiguous transport run becomes one BWF. Its TimeReference is the absolute
timeline position in samples since 00:00:00:00.

`cmd/ptslverify` creates a throwaway Pro Tools session (01:00:00:00, 48 kHz),
records a simulated take, delivers it, imports through PTSL, reads back where
Pro Tools placed each clip, and then closes the session without saving. Results
against Pro Tools 2026.4:

- **Clip placement.** Session +96,000 and session +484,321 (deliberately not on
  a frame boundary) landed exactly: the clips read back as 96000..144000 and
  484321..508321.
- **Pro Tools' own reading of our files.** `original_timestamp_point` equals the
  `bext` TimeReference we wrote for every clip (172,896,000 and 173,284,321). So
  the standard "spot to original time stamp" route also works with our files,
  not only the explicit PTSL spot.
- **PTSL position units.** PTSL sample positions are relative to the session
  start, not absolute. Selection at session start reads 0. The code converts at
  the PTSL boundary only.

Offline tests (`internal/deliver/e2e_test.go`) cover the following, each through
real FFmpeg:

- A known timeline with start, stop, locate, and a loop jump.
- A simulated capture latency of 37 frames.
- A crash mid-take.
- An input dropout.

Every delivered sample is compared with the signal value at the timeline
position implied by the file's own TimeReference, using readers independent of
FFmpeg. All match exactly.

### 4. PTSL integration: what it can and cannot do

PTSL can:

- Read session name, sample rate, timecode rate, start time, transport state,
  timeline selection, and system delay.
- Create and close sessions, import, and spot.

PTSL cannot:

- Report the playhead position.
- Notify on transport changes.
- Provide per-buffer timing.

Measured `GetTransportState` round trip over 200 calls: min 3.9 ms, median
8.1 ms, p95 12.2 ms, max 94.6 ms. That is 186 to 4,539 samples at 48 kHz.

Consequence: transport events obtained by polling PTSL are stamped at the
midpoint of the polling window and carry a declared uncertainty, which
`deliver` reports. **Placement of live takes from PTSL polling is not
sample-accurate and is not claimed to be.** Delivery itself is sample-exact for
whatever anchors it is given.

Operational findings:

- Every request needs a version header, `2026.4.0` here. Without it Pro Tools
  returns `SDK_VersionMismatch`, even for `GetPTSLVersion`.
- On first contact, Pro Tools 2026.4 crashed (Access Violation in UI startup)
  at the moment a request arrived during initialization. The client now waits
  for `HostReadyCheck` before sending anything else. Since then there have been
  6 sessions with no crash. The causal link is likely but not proven.
- Enum values have multiple aliases (`TS_TransportPlaying` and
  `TState_TransportPlaying`; `SSTCRate_Fps2997Drop`). Values are matched on
  the part shared by all aliases.
- The client's envelope encoding and every command ID are checked against the
  descriptor shipped in the installed Pro Tools (`ptsl_wire_test.go`). The
  schema is read at test time and is not redistributed.

### 5. Positions are kept distinct

Code that converts between these is the only place they meet:

- **Master frame:** index into `master.caf`.
- **Device sample time and host time:** the capture clock, in `capture.log`.
- **Timeline sample:** absolute, since 00:00:00:00. Session offset = timeline
  sample − session start.
- **Transport frame:** the master frame at which the DAW transport event
  occurred. Audio arrives `latency_frames` later. Dropout re-anchoring
  translates gaps from master frames to transport frames (a bug caught by
  `TestDropoutIsReanchored`).

## Blockers and risks

1. **No sample-accurate live transport anchor for Pro Tools.** PTSL is too
   coarse (see 4). Per-buffer session positions require an AAX plugin reading
   the host transport. The AAX SDK is not on this machine, and running in
   retail Pro Tools requires Avid developer access and PACE signing. Needed from
   the owner: Avid developer account, AAX SDK, and PACE/iLok signing
   credentials (or a Pro Tools Developer build). The journal format already
   accepts exact frame-stamped events (`take.Event.Frame`) from such a source.
2. **Capture-path latency** (`latency_frames`) is configurable but not yet
   measured per interface. It needs a round-trip measurement with the real
   interface, and `GetSessionSystemDelayInfo` is available to help.
3. **Durability on power loss**: no fsync of the master yet (see 2).
4. **Sample-rate mismatch** between device and session is refused at delivery
   rather than resampled. This is deliberate, but there is no UI warning at
   record time yet.

## Not started (by design, per the phase plan)

- Live monitoring transport (Opus/UDP or alternatives): Phase 3.
- Session-aware cache: Phase 4.
- Collaboration, authentication, UI: Phase 5.
