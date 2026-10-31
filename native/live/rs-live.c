// rs-live: low-latency monitoring audio. Opus in, Opus out.
//
//   rs-live encode --rate R [--channels 2] [--frame 480] [--bitrate 256000]
//       stdin:  interleaved float32 at R Hz (resampled to 48 kHz if R is
//               not an Opus rate)
//       stdout: packets
//
//   rs-live play [--device NAME|UID] [--ch L,R] [--target-ms 40] [--min-ms 20]
//                [--null PPM] [--dump FILE]
//       stdin:  packets
//       stderr: a JSON header, then one JSON stats line per second
//
// Packet: [u32 BE length][u32 BE sequence][Opus frame], length covering
// sequence and frame. The first packet carries the stream format in a
// "hello" packet with sequence 0xFFFFFFFF and a JSON body
// {"rate":48000,"channels":2,"frame":480}.
//
// play keeps a jitter buffer of packets ahead of playback and holds its
// depth at a target with a resampler whose ratio moves by at most 0.1 %
// (1.7 cents):
// that absorbs the clock difference between the two machines (and any
// device rate other than 48 kHz) without clicks. A missing packet is
// concealed with Opus PLC only when playback would otherwise run dry.
// Late packets are dropped. When the depth is far off target (a burst
// after a stall) whole packets are skipped instead of waiting for the
// resampler. If nothing arrives at all, it stops concealing after five
// packets and rebuffers. Underruns and
// late packets raise the target by 10 ms (up to 300); 30 s without either
// lowers it by 5 ms (down to --min-ms).
//
// The CoreAudio IOProc only reads a lock-free ring. --null replaces the
// device with a clock running PPM parts per million fast (or slow), for
// tests; --dump writes what was played as float32.
#include <CoreAudio/CoreAudio.h>
#include <math.h>
#include <opus.h>
#include <pthread.h>
#include <pthread/qos.h>
#include <mach/mach.h>
#include <mach/mach_time.h>
#include <mach/thread_policy.h>
#include <signal.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#define OPUS_RATE 48000
#define MAX_CH 2
#define MAX_FRAME 960 // 20 ms
#define MAX_PACKET 4000
#define HELLO_SEQ 0xFFFFFFFFu

static volatile sig_atomic_t quit;
static void on_signal(int s) { (void)s; quit = 1; }

static double now_s(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (double)ts.tv_sec + ts.tv_nsec / 1e9;
}

static void sleep_ms(double ms) {
    struct timespec ts = {(time_t)(ms / 1000), (long)(fmod(ms, 1000) * 1e6)};
    nanosleep(&ts, NULL);
}

static int read_full(FILE *f, void *p, size_t n) { return fread(p, 1, n, f) == n; }

static uint32_t be32(const uint8_t *b) { return (uint32_t)b[0] << 24 | (uint32_t)b[1] << 16 | (uint32_t)b[2] << 8 | b[3]; }
static void put_be32(uint8_t *b, uint32_t v) { b[0] = v >> 24; b[1] = v >> 16; b[2] = v >> 8; b[3] = v; }

// ---------------------------------------------------------------------
// Resampler: windowed sinc (Blackman), 2×16 taps, 512 phases with linear
// interpolation between them. step = input frames per output frame; it
// may change between calls (drift control).

#define RS_HALF 16
#define RS_PHASES 512

typedef struct {
    int ch;
    float table[(RS_PHASES + 1) * 2 * RS_HALF];
    double cutoff;
    float *buf;     // interleaved input history
    size_t len, cap; // frames
    double pos;     // position of the next output in buf (frames)
} resampler;

static void rs_init(resampler *r, int ch, double max_step) {
    memset(r, 0, sizeof *r);
    r->ch = ch;
    // Downsampling needs the cutoff lowered below the output Nyquist.
    r->cutoff = 0.97 * (max_step > 1 ? 1 / max_step : 1);
    for (int p = 0; p <= RS_PHASES; p++) {
        double frac = (double)p / RS_PHASES;
        for (int k = 0; k < 2 * RS_HALF; k++) {
            double x = (k - (RS_HALF - 1)) - frac; // distance from the output point
            double w = 0.42 + 0.5 * cos(M_PI * x / RS_HALF) + 0.08 * cos(2 * M_PI * x / RS_HALF);
            if (fabs(x) >= RS_HALF) w = 0;
            double s = x == 0 ? 1 : sin(M_PI * x * r->cutoff) / (M_PI * x * r->cutoff);
            r->table[p * 2 * RS_HALF + k] = (float)(r->cutoff * s * w);
        }
    }
    r->cap = 8192;
    r->buf = calloc(r->cap * ch, sizeof(float));
    r->len = RS_HALF; // zero history so the first output has its left taps
    r->pos = RS_HALF - 1;
}

static void rs_push(resampler *r, const float *in, size_t frames) {
    if (r->len + frames > r->cap) {
        while (r->len + frames > r->cap) r->cap *= 2;
        r->buf = realloc(r->buf, r->cap * r->ch * sizeof(float));
    }
    memcpy(r->buf + r->len * r->ch, in, frames * r->ch * sizeof(float));
    r->len += frames;
}

// rs_pull writes up to max output frames; returns how many.
static size_t rs_pull(resampler *r, float *out, size_t max, double step) {
    size_t n = 0;
    while (n < max) {
        size_t i = (size_t)r->pos;
        if (i + RS_HALF + 1 >= r->len) break; // need right-hand taps
        double frac = r->pos - i;
        double fp = frac * RS_PHASES;
        int p = (int)fp;
        float t = (float)(fp - p);
        const float *c0 = r->table + p * 2 * RS_HALF, *c1 = c0 + 2 * RS_HALF;
        const float *src = r->buf + (i - (RS_HALF - 1)) * r->ch;
        for (int c = 0; c < r->ch; c++) {
            float acc = 0;
            for (int k = 0; k < 2 * RS_HALF; k++) acc += src[k * r->ch + c] * (c0[k] + t * (c1[k] - c0[k]));
            out[n * r->ch + c] = acc;
        }
        n++;
        r->pos += step;
    }
    // Drop input no longer needed.
    size_t drop = (size_t)r->pos;
    drop = drop > RS_HALF ? drop - RS_HALF : 0;
    if (drop) {
        memmove(r->buf, r->buf + drop * r->ch, (r->len - drop) * r->ch * sizeof(float));
        r->len -= drop;
        r->pos -= drop;
    }
    return n;
}

// rs_ahead is how many input frames are buffered beyond the read point.
static double rs_ahead(const resampler *r) { return r->len - r->pos; }

// ---------------------------------------------------------------------
// encode

static int is_opus_rate(int r) { return r == 8000 || r == 12000 || r == 16000 || r == 24000 || r == 48000; }

static void write_packet(uint32_t seq, const uint8_t *p, int n) {
    uint8_t h[8];
    put_be32(h, (uint32_t)n + 4);
    put_be32(h + 4, seq);
    fwrite(h, 1, 8, stdout);
    fwrite(p, 1, (size_t)n, stdout);
}

static int cmd_encode(int argc, char **argv) {
    int rate = 0, ch = 2, frame = 480, bitrate = 256000;
    for (int i = 0; i < argc; i++) {
        if (!strcmp(argv[i], "--rate") && i + 1 < argc) rate = atoi(argv[++i]);
        else if (!strcmp(argv[i], "--channels") && i + 1 < argc) ch = atoi(argv[++i]);
        else if (!strcmp(argv[i], "--frame") && i + 1 < argc) frame = atoi(argv[++i]);
        else if (!strcmp(argv[i], "--bitrate") && i + 1 < argc) bitrate = atoi(argv[++i]);
        else { fprintf(stderr, "rs-live encode: unknown argument %s\n", argv[i]); return 1; }
    }
    if (rate <= 0 || ch < 1 || ch > MAX_CH || (frame != 120 && frame != 240 && frame != 480 && frame != 960)) {
        fprintf(stderr, "rs-live encode: need --rate, 1-2 channels, --frame 120/240/480/960 (at 48 kHz)\n");
        return 1;
    }
    int orate = is_opus_rate(rate) ? rate : OPUS_RATE;
    int oframe = frame * orate / OPUS_RATE;
    int err;
    OpusEncoder *enc = opus_encoder_create(orate, ch, OPUS_APPLICATION_RESTRICTED_LOWDELAY, &err);
    if (err != OPUS_OK) { fprintf(stderr, "rs-live: opus: %s\n", opus_strerror(err)); return 1; }
    opus_encoder_ctl(enc, OPUS_SET_BITRATE(bitrate));
    opus_encoder_ctl(enc, OPUS_SET_COMPLEXITY(8));
    opus_encoder_ctl(enc, OPUS_SET_SIGNAL(OPUS_SIGNAL_MUSIC));

    char hello[128];
    int hn = snprintf(hello, sizeof hello, "{\"rate\":%d,\"channels\":%d,\"frame\":%d}", orate, ch, oframe);
    write_packet(HELLO_SEQ, (uint8_t *)hello, hn);
    fflush(stdout);

    resampler rs;
    int resample = orate != rate;
    double step = (double)rate / orate;
    if (resample) rs_init(&rs, ch, step);
    float in[1024 * MAX_CH], pcm[MAX_FRAME * MAX_CH];
    size_t have = 0;
    uint8_t pkt[MAX_PACKET];
    uint32_t seq = 0;
    signal(SIGPIPE, SIG_IGN);
    for (;;) {
        size_t n = fread(in, sizeof(float) * ch, 1024, stdin);
        if (n == 0) break;
        size_t used = 0;
        if (resample) rs_push(&rs, in, n);
        for (;;) {
            size_t got;
            if (resample) {
                got = rs_pull(&rs, pcm + have * ch, (size_t)oframe - have, step);
            } else {
                got = n - used < (size_t)oframe - have ? n - used : (size_t)oframe - have;
                memcpy(pcm + have * ch, in + used * ch, got * ch * sizeof(float));
                used += got;
            }
            have += got;
            if (have < (size_t)oframe) break;
            int len = opus_encode_float(enc, pcm, oframe, pkt, sizeof pkt);
            if (len < 0) { fprintf(stderr, "rs-live: encode: %s\n", opus_strerror(len)); return 1; }
            write_packet(seq++, pkt, len);
            if (fflush(stdout) != 0) return 0; // reader gone
            have = 0;
        }
    }
    return 0;
}

// ---------------------------------------------------------------------
// play

#define SLOTS 1024 // packets the jitter buffer can hold (10 s at 10 ms)

typedef struct {
    int present;
    uint32_t seq;
    int len;
    uint8_t data[MAX_PACKET];
} slot;

static struct {
    // Stream format (from hello).
    int rate, ch, frame;
    // Jitter buffer, guarded by mu.
    pthread_mutex_t mu;
    slot slots[SLOTS];
    int64_t newest; // highest sequence received, -1 none
    int64_t next;   // next sequence the decoder wants, -1 before start
    int eof;
    // Output ring, single producer (decoder) / single consumer (IOProc).
    float *ring;
    size_t ring_cap; // frames
    _Atomic size_t head, tail;
    int out_ch, left, right;
    double out_rate;
    // Stats.
    _Atomic long underruns;
    _Atomic int draining; // stream ended: running dry is expected
    long received, late, plc, skipped;
} P = {.newest = -1, .next = -1};

static size_t ring_fill(void) { return atomic_load(&P.head) - atomic_load(&P.tail); }

// ring_read is called from the IOProc (or the null clock): no locks.
static void ring_read(float *out, size_t frames, int stride, int left, int right) {
    size_t tail = atomic_load_explicit(&P.tail, memory_order_relaxed);
    size_t head = atomic_load_explicit(&P.head, memory_order_acquire);
    size_t avail = head - tail;
    size_t n = avail < frames ? avail : frames;
    for (size_t f = 0; f < n; f++) {
        const float *s = P.ring + ((tail + f) % P.ring_cap) * 2;
        if (stride) {
            out[f * stride + left] = s[0];
            out[f * stride + right] = s[1];
        }
    }
    if (n < frames && !atomic_load_explicit(&P.draining, memory_order_relaxed))
        atomic_fetch_add_explicit(&P.underruns, 1, memory_order_relaxed);
    atomic_store_explicit(&P.tail, tail + n, memory_order_release);
}

static void ring_write(const float *in, size_t frames, int ch) {
    size_t head = atomic_load_explicit(&P.head, memory_order_relaxed);
    for (size_t f = 0; f < frames; f++) {
        float *d = P.ring + ((head + f) % P.ring_cap) * 2;
        d[0] = in[f * ch];
        d[1] = in[f * ch + (ch > 1)];
    }
    atomic_store_explicit(&P.head, head + frames, memory_order_release);
}

static OSStatus io_proc(AudioObjectID dev, const AudioTimeStamp *now, const AudioBufferList *in,
                        const AudioTimeStamp *in_time, AudioBufferList *out, const AudioTimeStamp *out_time,
                        void *ctx) {
    (void)dev; (void)now; (void)in; (void)in_time; (void)out_time; (void)ctx;
    // Interleaved float output in the first buffer (the HAL's canonical
    // format); other buffers are silenced.
    for (UInt32 b = 0; b < out->mNumberBuffers; b++)
        memset(out->mBuffers[b].mData, 0, out->mBuffers[b].mDataByteSize);
    if (out->mNumberBuffers == 0) return noErr;
    AudioBuffer *ab = &out->mBuffers[0];
    int stride = (int)ab->mNumberChannels;
    size_t frames = ab->mDataByteSize / sizeof(float) / (size_t)stride;
    if (P.left >= stride || P.right >= stride) { ring_read(NULL, frames, 0, 0, 0); return noErr; }
    ring_read((float *)ab->mData, frames, stride, P.left, P.right);
    return noErr;
}

static void *reader(void *arg) {
    (void)arg;
    uint8_t h[8];
    for (;;) {
        if (!read_full(stdin, h, 8)) break;
        uint32_t len = be32(h), seq = be32(h + 4);
        if (len < 4 || len - 4 > MAX_PACKET) { fprintf(stderr, "rs-live: bad packet length %u\n", len); break; }
        uint8_t data[MAX_PACKET];
        if (!read_full(stdin, data, len - 4)) break;
        if (seq == HELLO_SEQ) continue; // format already known
        pthread_mutex_lock(&P.mu);
        P.received++;
        if (P.next >= 0 && (int64_t)seq < P.next) { // its turn has passed
            P.late++;
            pthread_mutex_unlock(&P.mu);
            continue;
        }
        slot *s = &P.slots[seq % SLOTS];
        s->present = 1;
        s->seq = seq;
        s->len = (int)len - 4;
        memcpy(s->data, data, len - 4);
        if ((int64_t)seq > P.newest) P.newest = seq;
        pthread_mutex_unlock(&P.mu);
    }
    pthread_mutex_lock(&P.mu);
    P.eof = 1;
    pthread_mutex_unlock(&P.mu);
    return NULL;
}

// null_clock consumes the ring like a device whose clock runs ppm fast,
// writing what it plays to dump.
static double null_ppm;
static FILE *dump;
// make_realtime gives the calling thread time-constraint scheduling, as
// CoreAudio gives its IO threads: period and constraint in ms, with a
// computation budget of 1 ms.
static void make_realtime(double period_ms) {
    mach_timebase_info_data_t tb;
    mach_timebase_info(&tb);
    double per_ms = 1e6 * tb.denom / tb.numer;
    thread_time_constraint_policy_data_t pol = {(uint32_t)(period_ms * per_ms), (uint32_t)(1 * per_ms),
                                                (uint32_t)(period_ms * per_ms), 1};
    thread_policy_set(pthread_mach_thread_np(pthread_self()), THREAD_TIME_CONSTRAINT_POLICY, (thread_policy_t)&pol,
                      THREAD_TIME_CONSTRAINT_POLICY_COUNT);
}

static void *null_clock(void *arg) {
    (void)arg;
    // So the simulated device keeps time like a real one.
    make_realtime(5.3);
    const size_t chunk = 256;
    float buf[256 * 2];
    double rate = P.out_rate * (1 + null_ppm / 1e6);
    double start = now_s(), played = 0;
    while (!quit) {
        double due = start + (played + chunk) / rate;
        double wait = due - now_s();
        if (wait > 0) sleep_ms(wait * 1000);
        else if (wait < -0.05) { // a stall a device would not have: do not burst to catch up
            start -= wait;
            if (getenv("RS_LIVE_TRACE")) fprintf(stderr, "trace: null clock stalled %.0f ms\n", -wait * 1000);
        }
        memset(buf, 0, sizeof buf);
        size_t before = ring_fill();
        ring_read(buf, chunk, 2, 0, 1);
        if (getenv("RS_LIVE_TRACE") && before < chunk)
            fprintf(stderr, "trace: underrun at %.3f s, ring %zu, next %lld newest %lld\n", now_s() - start, before,
                    (long long)P.next, (long long)P.newest);
        if (dump) fwrite(buf, sizeof(float) * 2, chunk, dump);
        played += chunk;
    }
    return NULL;
}

static AudioObjectID find_output(const char *want) {
    AudioObjectPropertyAddress a = {want ? kAudioHardwarePropertyDevices : kAudioHardwarePropertyDefaultOutputDevice,
                                    kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
    UInt32 size = 0;
    if (!want) {
        AudioObjectID d = 0;
        size = sizeof d;
        AudioObjectGetPropertyData(kAudioObjectSystemObject, &a, 0, NULL, &size, &d);
        return d;
    }
    AudioObjectGetPropertyDataSize(kAudioObjectSystemObject, &a, 0, NULL, &size);
    AudioObjectID ids[256];
    if (size > sizeof ids) size = sizeof ids;
    AudioObjectGetPropertyData(kAudioObjectSystemObject, &a, 0, NULL, &size, ids);
    for (UInt32 i = 0; i < size / sizeof(AudioObjectID); i++) {
        CFStringRef s = NULL;
        UInt32 z = sizeof s;
        char name[256] = "", uid[256] = "";
        AudioObjectPropertyAddress na = {kAudioObjectPropertyName, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
        if (AudioObjectGetPropertyData(ids[i], &na, 0, NULL, &z, &s) == noErr && s) {
            CFStringGetCString(s, name, sizeof name, kCFStringEncodingUTF8);
            CFRelease(s);
        }
        na.mSelector = kAudioDevicePropertyDeviceUID;
        z = sizeof s;
        if (AudioObjectGetPropertyData(ids[i], &na, 0, NULL, &z, &s) == noErr && s) {
            CFStringGetCString(s, uid, sizeof uid, kCFStringEncodingUTF8);
            CFRelease(s);
        }
        if (!strcmp(want, name) || !strcmp(want, uid)) return ids[i];
    }
    return 0;
}

static int output_channels(AudioObjectID dev) {
    AudioObjectPropertyAddress a = {kAudioDevicePropertyStreamConfiguration, kAudioObjectPropertyScopeOutput, kAudioObjectPropertyElementMain};
    UInt32 size = 0;
    if (AudioObjectGetPropertyDataSize(dev, &a, 0, NULL, &size) != noErr || !size) return 0;
    AudioBufferList *bl = malloc(size);
    int n = 0;
    if (AudioObjectGetPropertyData(dev, &a, 0, NULL, &size, bl) == noErr && bl->mNumberBuffers)
        n = (int)bl->mBuffers[0].mNumberChannels; // first stream only
    free(bl);
    return n;
}

static int cmd_play(int argc, char **argv) {
    const char *device = NULL, *dump_path = NULL;
    double target_ms = 40, min_ms = 20;
    int use_null = 0, l = 1, r = 2;
    for (int i = 0; i < argc; i++) {
        if (!strcmp(argv[i], "--device") && i + 1 < argc) device = argv[++i];
        else if (!strcmp(argv[i], "--ch") && i + 1 < argc) sscanf(argv[++i], "%d,%d", &l, &r);
        else if (!strcmp(argv[i], "--target-ms") && i + 1 < argc) target_ms = atof(argv[++i]);
        else if (!strcmp(argv[i], "--min-ms") && i + 1 < argc) min_ms = atof(argv[++i]);
        else if (!strcmp(argv[i], "--null") && i + 1 < argc) { use_null = 1; null_ppm = atof(argv[++i]); }
        else if (!strcmp(argv[i], "--dump") && i + 1 < argc) dump_path = argv[++i];
        else { fprintf(stderr, "rs-live play: unknown argument %s\n", argv[i]); return 1; }
    }
    if (target_ms < min_ms) target_ms = min_ms;
    P.left = l - 1;
    P.right = r - 1;

    // The hello packet gives the format.
    uint8_t h[8];
    char hello[256] = "";
    if (!read_full(stdin, h, 8) || be32(h + 4) != HELLO_SEQ || be32(h) - 4 >= sizeof hello ||
        !read_full(stdin, hello, be32(h) - 4)) {
        fprintf(stderr, "rs-live play: stream does not start with a hello packet\n");
        return 1;
    }
    const char *p;
    if ((p = strstr(hello, "\"rate\":"))) P.rate = atoi(p + 7);
    if ((p = strstr(hello, "\"channels\":"))) P.ch = atoi(p + 11);
    if ((p = strstr(hello, "\"frame\":"))) P.frame = atoi(p + 8);
    if (!is_opus_rate(P.rate) || P.ch < 1 || P.ch > MAX_CH || P.frame <= 0 || P.frame > MAX_FRAME) {
        fprintf(stderr, "rs-live play: bad stream format %s\n", hello);
        return 1;
    }

    AudioObjectID dev = 0;
    if (use_null) {
        P.out_rate = P.rate;
        if (dump_path && !(dump = fopen(dump_path, "wb"))) { perror("rs-live: dump"); return 1; }
    } else {
        if (!(dev = find_output(device))) {
            fprintf(stderr, "rs-live play: no output device %s\n", device ? device : "(default)");
            return 1;
        }
        AudioObjectPropertyAddress ra = {kAudioDevicePropertyNominalSampleRate, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
        Float64 dr = 0;
        UInt32 z = sizeof dr;
        AudioObjectGetPropertyData(dev, &ra, 0, NULL, &z, &dr);
        P.out_rate = dr > 0 ? dr : 48000;
        P.out_ch = output_channels(dev);
        if (P.left >= P.out_ch || P.right >= P.out_ch) {
            fprintf(stderr, "rs-live play: the device has %d output channels\n", P.out_ch);
            return 1;
        }
    }
    P.ring_cap = (size_t)(P.out_rate * 2);
    P.ring = calloc(P.ring_cap * 2, sizeof(float));
    pthread_mutex_init(&P.mu, NULL);

    int err;
    OpusDecoder *dec = opus_decoder_create(P.rate, P.ch, &err);
    if (err != OPUS_OK) { fprintf(stderr, "rs-live: opus: %s\n", opus_strerror(err)); return 1; }
    double base_step = P.rate / P.out_rate;
    resampler rs;
    rs_init(&rs, P.ch, base_step * 1.001);

    signal(SIGINT, on_signal);
    signal(SIGTERM, on_signal);
    fprintf(stderr, "{\"rate\":%d,\"channels\":%d,\"frame\":%d,\"out_rate\":%.0f,\"target_ms\":%.0f}\n",
            P.rate, P.ch, P.frame, P.out_rate, target_ms);

    pthread_t rt, nt;
    pthread_create(&rt, NULL, reader, NULL);
    // The decoder must refill the ring within a few milliseconds. At
    // default priority it was measured missing 20 ms under load, and at
    // user-interactive QoS still missing it with a remote-desktop app
    // busy, so it is scheduled like an audio thread. It does well under
    // 1 ms of work per 10 ms packet and sleeps between.
    make_realtime(5);

    // Ring kept just deep enough for the device: what the decoder tops up
    // to on every pass. The jitter buffer proper is the packet queue.
    double ring_ms = 20; // counted in the depth, so it adds no latency of its own
    size_t ring_target = (size_t)(P.out_rate * ring_ms / 1000);
    if (!use_null) {
        AudioObjectPropertyAddress ba = {kAudioDevicePropertyBufferFrameSize, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
        UInt32 bf = 0, z = sizeof bf;
        AudioObjectGetPropertyData(dev, &ba, 0, NULL, &z, &bf);
        if (2 * bf > ring_target) ring_target = 2 * bf;
    }

    float pcm[MAX_FRAME * MAX_CH], out[2048 * MAX_CH];
    const double frame_ms = 1000.0 * P.frame / P.rate;
    const double max_adj = 0.001; // ±0.1 % (1.7 cents): drift only
    int64_t next = -1;            // next sequence to decode
    int clock_running = 0, buffering = 1, plc_run = 0;
    double depth_ema = -1, ratio_adj = 0, integral = 0;
    double t_prev = now_s(), last_event = -1e9, last_lower = now_s(), last_stats = now_s();
    long underruns_seen = 0, late_seen = 0;
    AudioDeviceIOProcID proc = NULL;

    while (!quit) {
        pthread_mutex_lock(&P.mu);
        int64_t newest = P.newest;
        int eof = P.eof;
        pthread_mutex_unlock(&P.mu);
        if (eof && (next < 0 || next > newest) && rs_ahead(&rs) < RS_HALF + 2) break;
        if (eof) atomic_store(&P.draining, 1);

        double t = now_s(), dt = t - t_prev;
        t_prev = t;
        // Depth: all audio queued ahead of the device, packets plus what
        // the resampler and the output ring hold.
        double depth_ms = next < 0 ? 0
                                   : fmax(0, (double)(newest - next + 1)) * frame_ms + rs_ahead(&rs) * 1000.0 / P.rate +
                                         ring_fill() * 1000.0 / P.out_rate;
        // Smoothed over 2 s: packets arrive in 10 ms steps plus jitter,
        // and the ratio must not follow that (it would wobble the pitch).
        depth_ema = depth_ema < 0 ? depth_ms : depth_ema + fmin(1, dt / 2.0) * (depth_ms - depth_ema);

        if (buffering) {
            // (Re)fill the jitter buffer to the target, starting from the
            // newest packet so a backlog is not played late.
            if (newest < 0) { sleep_ms(1); continue; }
            if (next < 0 || newest - next > (int64_t)(target_ms / frame_ms) + 1) {
                next = newest - (int64_t)(target_ms / frame_ms);
                if (next < 0) next = 0;
                pthread_mutex_lock(&P.mu);
                P.next = next;
                pthread_mutex_unlock(&P.mu);
                depth_ema = -1;
                continue;
            }
            if (depth_ms < target_ms) { sleep_ms(1); continue; }
            buffering = 0;
            plc_run = 0;
            depth_ema = depth_ms;
        }

        // Far too deep (a burst after a stall): skip whole packets.
        if (depth_ema > target_ms + 40 && newest - next > 1) {
            int64_t to = newest - (int64_t)(target_ms / frame_ms);
            if (to > next) {
                pthread_mutex_lock(&P.mu);
                for (int64_t q = next; q < to; q++) P.slots[q % SLOTS].present = 0;
                P.skipped += to - next;
                P.next = to;
                pthread_mutex_unlock(&P.mu);
                next = to;
                depth_ema = -1;
                continue;
            }
        }
        // Drift: PI control of the depth (seconds of surplus). The
        // integral settles at the clock difference, so the depth settles
        // on the target; damping 0.5, natural period about a minute
        // (1 ms off target moves the ratio 100 ppm, 0.17 cents).
        if (!eof) {
            double e = (depth_ema - target_ms) / 1000.0;
            integral = fmax(-max_adj, fmin(max_adj, integral + e * dt * 0.01));
            ratio_adj = fmax(-max_adj, fmin(max_adj, e * 0.1 + integral));
        }

        if (ring_fill() >= ring_target) {
            if (!clock_running) {
                clock_running = 1;
                // (prebuffer counted the ring, so the clock starts at the target)
                atomic_store(&P.underruns, 0);
                if (use_null) {
                    pthread_create(&nt, NULL, null_clock, NULL);
                } else if (AudioDeviceCreateIOProcID(dev, io_proc, NULL, &proc) != noErr || AudioDeviceStart(dev, proc) != noErr) {
                    fprintf(stderr, "rs-live play: cannot start the output device\n");
                    return 1;
                }
            }
            sleep_ms(0.5);
        } else {
            size_t want = ring_target + 64 - ring_fill();
            size_t got = rs_pull(&rs, out, want < 1024 ? want : 1024, base_step * (1 + ratio_adj));
            if (got) {
                ring_write(out, got, P.ch);
                continue;
            }
            // The resampler needs the next packet.
            int n = -1, have = 0, len = 0;
            uint8_t data[MAX_PACKET];
            pthread_mutex_lock(&P.mu);
            slot *sl = &P.slots[next % SLOTS];
            if (sl->present && sl->seq == (uint32_t)next) {
                have = 1;
                len = sl->len;
                memcpy(data, sl->data, (size_t)len);
                sl->present = 0;
            }
            pthread_mutex_unlock(&P.mu);
            int starving = ring_fill() < ring_target / 2 || !clock_running;
            if (have) {
                n = opus_decode_float(dec, data, len, pcm, P.frame, 0);
                plc_run = 0;
            } else if (starving && clock_running) {
                if (newest < next && plc_run >= 5) {
                    // Nothing is arriving: stop concealing and rebuffer.
                    buffering = 1;
                    next = -1;
                    continue;
                }
                n = opus_decode_float(dec, NULL, 0, pcm, P.frame, 0);
                P.plc++;
                plc_run++;
            } else {
                sleep_ms(0.5);
                continue;
            }
            next++;
            pthread_mutex_lock(&P.mu);
            P.next = next;
            pthread_mutex_unlock(&P.mu);
            if (n > 0) rs_push(&rs, pcm, (size_t)n);
        }

        // Headroom: an underrun or a packet that came after its turn raises
        // the target (at most every 0.5 s); 30 quiet seconds lower it. A
        // packet that never comes (loss) is concealed but does not raise
        // it: more buffer would not have helped.
        long u = atomic_load(&P.underruns);
        pthread_mutex_lock(&P.mu);
        long late = P.late;
        pthread_mutex_unlock(&P.mu);
        int event = !eof && ((clock_running && !buffering && u != underruns_seen) || late != late_seen);
        underruns_seen = u;
        late_seen = late;
        if (event && t - last_event > 0.5) {
            if (target_ms < 300) target_ms += 10;
            last_event = t;
            last_lower = t;
        } else if (t - last_event > 30 && t - last_lower > 30 && target_ms > min_ms) {
            target_ms = fmax(min_ms, target_ms - 5);
            last_lower = t;
        }
        if (t - last_stats >= 1) {
            last_stats = t;
            pthread_mutex_lock(&P.mu);
            fprintf(stderr, "{\"depth_ms\":%.1f,\"target_ms\":%.0f,\"ratio_ppm\":%.0f,\"received\":%ld,\"plc\":%ld,"
                            "\"late\":%ld,\"skipped\":%ld,\"underruns\":%ld,\"buffering\":%s,\"eof\":%s}\n",
                    depth_ema, target_ms, ratio_adj * 1e6, P.received, P.plc, P.late, P.skipped,
                    atomic_load(&P.underruns), buffering ? "true" : "false", eof ? "true" : "false");
            pthread_mutex_unlock(&P.mu);
        }
    }
    // Let the ring play out.
    while (!quit && clock_running && ring_fill() > 0) sleep_ms(5);
    quit = 1;
    if (proc) { AudioDeviceStop(dev, proc); AudioDeviceDestroyIOProcID(dev, proc); }
    if (use_null && clock_running) pthread_join(nt, NULL);
    if (dump) fclose(dump);
    fprintf(stderr, "{\"done\":true,\"received\":%ld,\"plc\":%ld,\"late\":%ld,\"skipped\":%ld,\"underruns\":%ld}\n",
            P.received, P.plc, P.late, P.skipped, atomic_load(&P.underruns));
    return 0;
}

// cmd_outputs lists output devices as JSON lines (name, uid, outputs,
// rate), the default output first.
static int cmd_outputs(void) {
    AudioObjectID def = find_output(NULL);
    AudioObjectPropertyAddress a = {kAudioHardwarePropertyDevices, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
    UInt32 size = 0;
    AudioObjectGetPropertyDataSize(kAudioObjectSystemObject, &a, 0, NULL, &size);
    AudioObjectID ids[256];
    if (size > sizeof ids) size = sizeof ids;
    AudioObjectGetPropertyData(kAudioObjectSystemObject, &a, 0, NULL, &size, ids);
    for (int pass = 0; pass < 2; pass++) {
        for (UInt32 i = 0; i < size / sizeof(AudioObjectID); i++) {
            if ((pass == 0) != (ids[i] == def)) continue;
            int n = output_channels(ids[i]);
            if (n <= 0) continue;
            char name[256] = "", uid[256] = "";
            CFStringRef s = NULL;
            UInt32 z = sizeof s;
            AudioObjectPropertyAddress na = {kAudioObjectPropertyName, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
            if (AudioObjectGetPropertyData(ids[i], &na, 0, NULL, &z, &s) == noErr && s) {
                CFStringGetCString(s, name, sizeof name, kCFStringEncodingUTF8);
                CFRelease(s);
            }
            na.mSelector = kAudioDevicePropertyDeviceUID;
            z = sizeof s;
            if (AudioObjectGetPropertyData(ids[i], &na, 0, NULL, &z, &s) == noErr && s) {
                CFStringGetCString(s, uid, sizeof uid, kCFStringEncodingUTF8);
                CFRelease(s);
            }
            AudioObjectPropertyAddress ra = {kAudioDevicePropertyNominalSampleRate, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
            Float64 r = 0;
            z = sizeof r;
            AudioObjectGetPropertyData(ids[i], &ra, 0, NULL, &z, &r);
            printf("{\"name\":\"");
            for (char *c = name; *c; c++) {
                if (*c == '"' || *c == '\\') putchar('\\');
                putchar(*c);
            }
            printf("\",\"uid\":\"");
            for (char *c = uid; *c; c++) {
                if (*c == '"' || *c == '\\') putchar('\\');
                putchar(*c);
            }
            printf("\",\"outputs\":%d,\"rate\":%.0f,\"default\":%s}\n", n, r, pass == 0 ? "true" : "false");
        }
    }
    return 0;
}

int main(int argc, char **argv) {
    if (argc >= 2 && !strcmp(argv[1], "outputs")) return cmd_outputs();
    if (argc >= 2 && !strcmp(argv[1], "encode")) return cmd_encode(argc - 2, argv + 2);
    if (argc >= 2 && !strcmp(argv[1], "play")) return cmd_play(argc - 2, argv + 2);
    fprintf(stderr, "usage: rs-live encode --rate R [--channels 2] [--frame 480] [--bitrate 256000]\n"
                    "       rs-live outputs\n"
                    "       rs-live play [--device NAME|UID] [--ch L,R] [--target-ms 40] [--min-ms 20] [--null PPM] [--dump FILE]\n");
    return 2;
}
