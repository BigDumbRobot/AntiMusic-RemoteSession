// Shared-memory layout between the RemoteSession Link plugin (writer) and
// the middle layer (reader, internal/link). Little-endian, fixed offsets;
// keep in sync with internal/link/link.go.
//
// File: <links dir>/<pid>-<instance>.rslink, memory-mapped by both sides.
//
//   [0, 4096)            header
//   [4096, +meta)        block metadata ring, kMetaSlots x 64 bytes
//   [.., +audio)         audio ring, capacityFrames x channels float32
//
// The writer never waits. Per block it writes the audio, then the block's
// metadata, then publishes by storing writeFrames and writeBlocks with
// release ordering. A reader that falls more than a ring behind detects
// it from those counters and resynchronizes; it re-checks writeFrames
// after copying to detect a block overwritten during the copy.
#pragma once

#include <atomic>
#include <cstdint>

namespace rslink
{
constexpr char kMagic[8] = { 'R', 'S', 'L', 'I', 'N', 'K', '0', '1' };
constexpr uint32_t kVersion = 1;
constexpr uint64_t kHeaderBytes = 4096;
constexpr uint64_t kMetaSlots = 8192;
constexpr uint64_t kMetaBytes = 64;

enum State : uint32_t { kStateActive = 1, kStateClosed = 2 };

enum BlockFlags : uint32_t
{
    kPlaying = 1u << 0,
    kRecording = 1u << 1,
    kHasTimeline = 1u << 2, // host supplied a sample position
};

struct Header
{
    char magic[8];                       // 0
    uint32_t version;                    // 8
    uint32_t channels;                   // 12
    double sampleRate;                   // 16
    uint64_t capacityFrames;             // 24
    uint64_t metaSlots;                  // 32
    uint32_t pid;                        // 40
    std::atomic<uint32_t> state;         // 44
    uint8_t pad0[16];                    // 48
    std::atomic<uint64_t> writeFrames;   // 64: frames published so far
    std::atomic<uint64_t> writeBlocks;   // 72: blocks published so far
    uint8_t pad1[48];                    // 80
    char trackName[256];                 // 128
    char hostName[64];                   // 384
};

struct BlockMeta
{
    uint64_t frame;          // 0: stream frame index of the block's first sample
    int64_t timelineSample;  // 8: host position of that sample (see flags)
    uint64_t hostMicros;     // 16: mach_absolute_time at processBlock, µs
    uint32_t frames;         // 24
    uint32_t flags;          // 28
    double sampleRate;       // 32: rate in effect for this block
    uint8_t pad[24];         // 40
};

static_assert (sizeof (Header) <= kHeaderBytes);
static_assert (sizeof (BlockMeta) == kMetaBytes);
static_assert (offsetof (Header, writeFrames) == 64);
static_assert (offsetof (Header, writeBlocks) == 72);
static_assert (offsetof (Header, trackName) == 128);
static_assert (std::atomic<uint64_t>::is_always_lock_free);
} // namespace rslink
