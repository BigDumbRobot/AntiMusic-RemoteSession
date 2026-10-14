// rs-capture: real-time-safe CoreAudio input capture.
//
// Reads every input channel of one CoreAudio device and writes them as
// interleaved little-endian float32 to stdout, for FFmpeg to record. It
// exists because FFmpeg's avfoundation input drops whole capture buffers
// (measured: see docs/ARCHITECTURE.md).
//
// The IOProc only copies into lock-free single-producer/single-consumer
// rings: no allocation, locks, logging or syscalls on the audio thread.
// A writer thread drains the rings to stdout and writes a capture log
// (--log) in the format the recorder's timeline package parses:
//
//   <master frame> <frames> <host µs> 1/1000000 <device sample time> 1/<rate> <filled>
//
// If an IO cycle is lost (ring overflow or a HAL discontinuity), the
// device sample time jumps; the writer fills the gap with silence so that
// master frames stay locked to the device clock, and marks the line with
// filled=1.
//
// --play FILE (test rigs only) also plays FILE, interleaved float32 with
// the device's output channel count, through the same IOProc, so a
// loopback device can be verified with a single client and clock.
//
// stderr: first line is a JSON header {"rate":..,"channels":..,...};
// diagnostics follow. Exit status: 0 on SIGINT/SIGTERM or stdout closed,
// 1 on setup errors, 3 if the device stops, changes rate or disappears.
#include <CoreAudio/CoreAudio.h>
#include <mach/mach_time.h>
#include <pthread.h>
#include <signal.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <math.h>

#define MAX_CHANNELS 256
#define META_SLOTS 4096

typedef struct {
    int64_t sample_time;
    uint64_t host_time;
    uint32_t frames;
} Meta;

static struct {
    // Sample ring (interleaved float32), capacity in samples.
    float *data;
    size_t cap;
    _Atomic size_t head, tail; // written by IOProc / writer respectively
    Meta meta[META_SLOTS];
    _Atomic size_t mhead, mtail;
    _Atomic uint64_t overflow_cycles;
    _Atomic int stop;
    _Atomic int device_fault;
    int channels;
    // Per input stream: channel count and whether it is usable.
    int nstreams;
    int stream_channels[64];
    // Optional test playback, preloaded before the device starts.
    const float *play;
    size_t play_frames, play_pos;
    int play_channels;
} g;

static mach_timebase_info_data_t timebase;

static uint64_t host_to_us(uint64_t t) {
    return (uint64_t)((__uint128_t)t * timebase.numer / timebase.denom / 1000);
}

static OSStatus io_proc(AudioObjectID dev, const AudioTimeStamp *now,
                        const AudioBufferList *in, const AudioTimeStamp *in_time,
                        AudioBufferList *out, const AudioTimeStamp *out_time, void *ctx) {
    (void)dev; (void)now; (void)out_time; (void)ctx;
    if (g.play && out) {
        int base = 0;
        size_t pos = g.play_pos;
        for (UInt32 b = 0; b < out->mNumberBuffers; b++) {
            AudioBuffer *ob = &out->mBuffers[b];
            int sc = (int)ob->mNumberChannels;
            uint32_t of = ob->mDataByteSize / (sizeof(float) * (sc ? sc : 1));
            float *dst = ob->mData;
            for (uint32_t f = 0; f < of && dst; f++)
                for (int c = 0; c < sc; c++)
                    dst[(size_t)f * sc + c] = (pos + f < g.play_frames && base + c < g.play_channels)
                        ? g.play[(pos + f) * g.play_channels + base + c] : 0.0f;
            base += sc;
            if (b + 1 == out->mNumberBuffers) g.play_pos = pos + of;
        }
    }
    if (!in || in->mNumberBuffers == 0) return noErr;
    uint32_t frames = in->mBuffers[0].mDataByteSize / (sizeof(float) * in->mBuffers[0].mNumberChannels);
    size_t need = (size_t)frames * g.channels;
    size_t head = atomic_load_explicit(&g.head, memory_order_relaxed);
    size_t tail = atomic_load_explicit(&g.tail, memory_order_acquire);
    size_t mhead = atomic_load_explicit(&g.mhead, memory_order_relaxed);
    size_t mtail = atomic_load_explicit(&g.mtail, memory_order_acquire);
    if (g.cap - (head - tail) < need || mhead - mtail >= META_SLOTS) {
        // Writer is behind: drop the whole cycle. The sample-time jump
        // tells the writer exactly how much to fill.
        atomic_fetch_add_explicit(&g.overflow_cycles, 1, memory_order_relaxed);
        return noErr;
    }
    // Interleave all streams' channels into the ring.
    int chan_base = 0;
    for (UInt32 b = 0; b < in->mNumberBuffers && b < (UInt32)g.nstreams; b++) {
        const AudioBuffer *buf = &in->mBuffers[b];
        const float *src = (const float *)buf->mData;
        int sc = (int)buf->mNumberChannels;
        for (uint32_t f = 0; f < frames; f++) {
            size_t base = head + (size_t)f * g.channels + chan_base;
            for (int c = 0; c < sc; c++) {
                g.data[(base + c) % g.cap] = src ? src[(size_t)f * sc + c] : 0.0f;
            }
        }
        chan_base += sc;
    }
    Meta *m = &g.meta[mhead % META_SLOTS];
    m->sample_time = (int64_t)llround(in_time->mSampleTime);
    m->host_time = in_time->mHostTime;
    m->frames = frames;
    atomic_store_explicit(&g.head, head + need, memory_order_release);
    atomic_store_explicit(&g.mhead, mhead + 1, memory_order_release);
    return noErr;
}

static OSStatus on_property(AudioObjectID obj, UInt32 n, const AudioObjectPropertyAddress *addrs, void *ctx) {
    (void)obj; (void)n; (void)addrs; (void)ctx;
    atomic_store(&g.device_fault, 1);
    return noErr;
}

static void on_signal(int sig) { (void)sig; atomic_store(&g.stop, 1); }

static int get_string(AudioObjectID id, AudioObjectPropertySelector sel, char *buf, size_t len) {
    AudioObjectPropertyAddress a = {sel, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
    CFStringRef s = NULL;
    UInt32 size = sizeof(s);
    if (AudioObjectGetPropertyData(id, &a, 0, NULL, &size, &s) != noErr || !s) return -1;
    Boolean ok = CFStringGetCString(s, buf, (CFIndex)len, kCFStringEncodingUTF8);
    CFRelease(s);
    return ok ? 0 : -1;
}

static int input_channels(AudioObjectID id) {
    AudioObjectPropertyAddress a = {kAudioDevicePropertyStreamConfiguration, kAudioObjectPropertyScopeInput, kAudioObjectPropertyElementMain};
    UInt32 size = 0;
    if (AudioObjectGetPropertyDataSize(id, &a, 0, NULL, &size) != noErr || size == 0) return 0;
    AudioBufferList *bl = malloc(size);
    int n = 0;
    if (AudioObjectGetPropertyData(id, &a, 0, NULL, &size, bl) == noErr)
        for (UInt32 i = 0; i < bl->mNumberBuffers; i++) n += (int)bl->mBuffers[i].mNumberChannels;
    free(bl);
    return n;
}

static double nominal_rate(AudioObjectID id) {
    AudioObjectPropertyAddress a = {kAudioDevicePropertyNominalSampleRate, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
    Float64 r = 0;
    UInt32 size = sizeof(r);
    AudioObjectGetPropertyData(id, &a, 0, NULL, &size, &r);
    return r;
}

static int list_devices(AudioObjectID **out) {
    AudioObjectPropertyAddress a = {kAudioHardwarePropertyDevices, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
    UInt32 size = 0;
    AudioObjectGetPropertyDataSize(kAudioObjectSystemObject, &a, 0, NULL, &size);
    *out = malloc(size);
    AudioObjectGetPropertyData(kAudioObjectSystemObject, &a, 0, NULL, &size, *out);
    return (int)(size / sizeof(AudioObjectID));
}

static void json_string(FILE *f, const char *s) {
    fputc('"', f);
    for (; *s; s++) {
        if (*s == '"' || *s == '\\') fputc('\\', f);
        if ((unsigned char)*s < 0x20) fprintf(f, "\\u%04x", *s);
        else fputc(*s, f);
    }
    fputc('"', f);
}

// Checks every input stream delivers packed, interleaved native float32
// and records per-stream channel counts.
static int check_streams(AudioObjectID dev) {
    AudioObjectPropertyAddress a = {kAudioDevicePropertyStreams, kAudioObjectPropertyScopeInput, kAudioObjectPropertyElementMain};
    UInt32 size = 0;
    if (AudioObjectGetPropertyDataSize(dev, &a, 0, NULL, &size) != noErr) return -1;
    int n = (int)(size / sizeof(AudioStreamID));
    if (n > 64) return -1;
    AudioStreamID ids[64];
    AudioObjectGetPropertyData(dev, &a, 0, NULL, &size, ids);
    g.nstreams = n;
    for (int i = 0; i < n; i++) {
        AudioStreamBasicDescription f;
        AudioObjectPropertyAddress fa = {kAudioStreamPropertyVirtualFormat, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
        UInt32 fs = sizeof(f);
        if (AudioObjectGetPropertyData(ids[i], &fa, 0, NULL, &fs, &f) != noErr) return -1;
        int is_float = (f.mFormatFlags & kAudioFormatFlagIsFloat) != 0;
        int nonint = (f.mFormatFlags & kAudioFormatFlagIsNonInterleaved) != 0;
        int big = (f.mFormatFlags & kAudioFormatFlagIsBigEndian) != 0;
        if (f.mFormatID != kAudioFormatLinearPCM || !is_float || f.mBitsPerChannel != 32 || nonint || big) {
            fprintf(stderr, "rs-capture: input stream %d is not interleaved native float32\n", i);
            return -1;
        }
        g.stream_channels[i] = (int)f.mChannelsPerFrame;
    }
    return 0;
}

static int write_all(int fd, const void *p, size_t n) {
    const char *c = p;
    while (n > 0) {
        ssize_t w = write(fd, c, n);
        if (w < 0) {
            if (errno == EINTR) continue;
            return -1;
        }
        c += w;
        n -= (size_t)w;
    }
    return 0;
}

int main(int argc, char **argv) {
    const char *want = NULL, *log_path = NULL, *play_path = NULL;
    int list = 0;
    double ring_seconds = 8;
    for (int i = 1; i < argc; i++) {
        if (!strcmp(argv[i], "--list")) list = 1;
        else if (!strcmp(argv[i], "--device") && i + 1 < argc) want = argv[++i];
        else if (!strcmp(argv[i], "--log") && i + 1 < argc) log_path = argv[++i];
        else if (!strcmp(argv[i], "--ring-seconds") && i + 1 < argc) ring_seconds = atof(argv[++i]);
        else if (!strcmp(argv[i], "--play") && i + 1 < argc) play_path = argv[++i];
        else {
            fprintf(stderr, "usage: rs-capture --list | --device NAME|UID [--log FILE] [--ring-seconds N]\n");
            return 1;
        }
    }
    mach_timebase_info(&timebase);
    AudioObjectID *ids;
    int n = list_devices(&ids);
    AudioObjectID dev = 0;
    char name[256], uid[256];
    for (int i = 0; i < n; i++) {
        int ch = input_channels(ids[i]);
        if (ch == 0) continue;
        if (get_string(ids[i], kAudioObjectPropertyName, name, sizeof name)) name[0] = 0;
        if (get_string(ids[i], kAudioDevicePropertyDeviceUID, uid, sizeof uid)) uid[0] = 0;
        if (list) {
            printf("{\"name\":");
            json_string(stdout, name);
            printf(",\"uid\":");
            json_string(stdout, uid);
            printf(",\"channels\":%d,\"rate\":%.0f}\n", ch, nominal_rate(ids[i]));
        } else if (want && (!strcmp(want, name) || !strcmp(want, uid))) {
            dev = ids[i];
            break;
        }
    }
    free(ids);
    if (list) return 0;
    if (!dev) {
        fprintf(stderr, "rs-capture: no input device named %s\n", want ? want : "(none)");
        return 1;
    }
    if (check_streams(dev)) return 1;
    g.channels = input_channels(dev);
    if (g.channels <= 0 || g.channels > MAX_CHANNELS) {
        fprintf(stderr, "rs-capture: unsupported channel count %d\n", g.channels);
        return 1;
    }
    double rate = nominal_rate(dev);
    g.cap = (size_t)(rate * ring_seconds) * g.channels;
    g.data = calloc(g.cap, sizeof(float));
    if (!g.data) return 1;
    // Fault on: rate change, device death, stream format change.
    AudioObjectPropertyAddress watch[] = {
        {kAudioDevicePropertyNominalSampleRate, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain},
        {kAudioDevicePropertyDeviceIsAlive, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain},
        {kAudioDevicePropertyStreamConfiguration, kAudioObjectPropertyScopeInput, kAudioObjectPropertyElementMain},
    };
    for (size_t i = 0; i < sizeof watch / sizeof watch[0]; i++)
        AudioObjectAddPropertyListener(dev, &watch[i], on_property, NULL);

    if (play_path) {
        AudioObjectPropertyAddress a = {kAudioDevicePropertyStreamConfiguration, kAudioObjectPropertyScopeOutput, kAudioObjectPropertyElementMain};
        UInt32 size = 0;
        AudioObjectGetPropertyDataSize(dev, &a, 0, NULL, &size);
        AudioBufferList *bl = malloc(size ? size : sizeof(AudioBufferList));
        g.play_channels = 0;
        if (size && AudioObjectGetPropertyData(dev, &a, 0, NULL, &size, bl) == noErr)
            for (UInt32 i = 0; i < bl->mNumberBuffers; i++) g.play_channels += (int)bl->mBuffers[i].mNumberChannels;
        free(bl);
        FILE *pf = fopen(play_path, "rb");
        if (!pf || g.play_channels == 0) {
            fprintf(stderr, "rs-capture: cannot play %s on a device with %d outputs\n", play_path, g.play_channels);
            return 1;
        }
        fseek(pf, 0, SEEK_END);
        long bytes = ftell(pf);
        fseek(pf, 0, SEEK_SET);
        float *buf = malloc((size_t)bytes);
        if (!buf || fread(buf, 1, (size_t)bytes, pf) != (size_t)bytes) return 1;
        fclose(pf);
        g.play = buf;
        g.play_frames = (size_t)bytes / sizeof(float) / (size_t)g.play_channels;
    }

    FILE *logf = NULL;
    if (log_path && !(logf = fopen(log_path, "w"))) {
        perror("rs-capture: log");
        return 1;
    }
    signal(SIGINT, on_signal);
    signal(SIGTERM, on_signal);
    signal(SIGPIPE, SIG_IGN);

    fprintf(stderr, "{\"rate\":%.0f,\"channels\":%d,\"name\":", rate, g.channels);
    json_string(stderr, name);
    fprintf(stderr, ",\"uid\":");
    json_string(stderr, uid);
    fprintf(stderr, "}\n");

    AudioDeviceIOProcID proc;
    if (AudioDeviceCreateIOProcID(dev, io_proc, NULL, &proc) != noErr || AudioDeviceStart(dev, proc) != noErr) {
        fprintf(stderr, "rs-capture: cannot start device\n");
        return 1;
    }

    // Writer loop.
    int status = 0;
    int64_t next_sample = INT64_MIN, master = 0;
    size_t chunk_max = 0;
    float *zeros = NULL;
    for (;;) {
        size_t mtail = atomic_load_explicit(&g.mtail, memory_order_relaxed);
        size_t mhead = atomic_load_explicit(&g.mhead, memory_order_acquire);
        if (mtail == mhead) {
            if (atomic_load(&g.stop)) break;
            if (atomic_load(&g.device_fault)) {
                fprintf(stderr, "rs-capture: device stopped, changed rate or format\n");
                status = 3;
                break;
            }
            usleep(2000);
            continue;
        }
        Meta m = g.meta[mtail % META_SLOTS];
        if (next_sample != INT64_MIN && m.sample_time != next_sample) {
            int64_t gap = m.sample_time - next_sample;
            if (gap < 0 || gap > (int64_t)(rate * 60)) {
                fprintf(stderr, "rs-capture: device clock jumped by %lld samples\n", (long long)gap);
                status = 3;
                break;
            }
            size_t gsamples = (size_t)gap * g.channels;
            if (gsamples > chunk_max) {
                free(zeros);
                zeros = calloc(gsamples, sizeof(float));
                chunk_max = gsamples;
            }
            if (write_all(1, zeros, gsamples * sizeof(float))) break;
            uint64_t gap_us = host_to_us(m.host_time) - (uint64_t)((double)gap * 1e6 / rate);
            if (logf) fprintf(logf, "%lld %lld %llu 1/1000000 %lld 1/%.0f 1\n", (long long)master, (long long)gap,
                              (unsigned long long)gap_us, (long long)next_sample, rate);
            fprintf(stderr, "rs-capture: filled %lld lost frames at master frame %lld\n", (long long)gap, (long long)master);
            master += gap;
        }
        size_t samples = (size_t)m.frames * g.channels;
        size_t tail = atomic_load_explicit(&g.tail, memory_order_relaxed);
        size_t start = tail % g.cap;
        size_t first = samples < g.cap - start ? samples : g.cap - start;
        if (write_all(1, g.data + start, first * sizeof(float)) ||
            write_all(1, g.data, (samples - first) * sizeof(float)))
            break; // stdout closed: recorder is gone
        atomic_store_explicit(&g.tail, tail + samples, memory_order_release);
        atomic_store_explicit(&g.mtail, mtail + 1, memory_order_release);
        if (logf) fprintf(logf, "%lld %u %llu 1/1000000 %lld 1/%.0f 0\n", (long long)master, m.frames,
                          (unsigned long long)host_to_us(m.host_time), (long long)m.sample_time, rate);
        if (logf && (mtail & 63) == 0) fflush(logf);
        master += m.frames;
        next_sample = m.sample_time + m.frames;
    }
    AudioDeviceStop(dev, proc);
    AudioDeviceDestroyIOProcID(dev, proc);
    if (logf) fclose(logf);
    uint64_t ov = atomic_load(&g.overflow_cycles);
    fprintf(stderr, "rs-capture: stopped after %lld frames; %llu IO cycles overflowed the ring\n",
            (long long)master, (unsigned long long)ov);
    return status;
}
