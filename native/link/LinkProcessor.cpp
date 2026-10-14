#include "LinkProcessor.h"

#include <fcntl.h>
#include <mach/mach_time.h>
#include <sys/mman.h>
#include <unistd.h>

#include <cstring>

namespace
{
std::atomic<int> instanceCounter { 0 };
constexpr double kRingSeconds = 10.0;
} // namespace

LinkProcessor::LinkProcessor()
    : AudioProcessor (BusesProperties()
                          .withInput ("Input", juce::AudioChannelSet::stereo(), true)
                          .withOutput ("Output", juce::AudioChannelSet::stereo(), true))
{
    mach_timebase_info_data_t tb;
    mach_timebase_info (&tb);
    ticksToMicros = (double) tb.numer / (double) tb.denom / 1000.0;
}

LinkProcessor::~LinkProcessor() { closeRing(); }

juce::File LinkProcessor::linksDirectory()
{
    if (auto* dir = std::getenv ("RS_LINK_DIR"))
        return juce::File (juce::String::fromUTF8 (dir));
    return juce::File::getSpecialLocation (juce::File::userApplicationDataDirectory)
        .getChildFile ("AntiMusic/RemoteSession/links");
}

bool LinkProcessor::isBusesLayoutSupported (const BusesLayout& layouts) const
{
    const auto& in = layouts.getMainInputChannelSet();
    const auto& out = layouts.getMainOutputChannelSet();
    return ! in.isDisabled() && in == out && in.size() <= 64;
}

void LinkProcessor::prepareToPlay (double sampleRate, int)
{
    const auto ch = getTotalNumInputChannels();
    if (header == nullptr || rate != sampleRate || channels != (uint32_t) ch)
    {
        closeRing();
        openRing (sampleRate, ch);
    }
}

void LinkProcessor::releaseResources() {}

void LinkProcessor::openRing (double sampleRate, int ch)
{
    rate = sampleRate;
    channels = (uint32_t) ch;
    capacityFrames = (uint64_t) (sampleRate * kRingSeconds);
    mappingBytes = rslink::kHeaderBytes + rslink::kMetaSlots * rslink::kMetaBytes
                   + capacityFrames * channels * sizeof (float);

    auto dir = linksDirectory();
    if (! dir.createDirectory())
        return;
    ringFile = dir.getChildFile (juce::String (getpid()) + "-" + juce::String (++instanceCounter) + ".rslink");
    fd = ::open (ringFile.getFullPathName().toRawUTF8(), O_RDWR | O_CREAT | O_TRUNC, 0644);
    if (fd < 0)
        return;
    if (::ftruncate (fd, (off_t) mappingBytes) != 0)
    {
        closeRing();
        return;
    }
    mapping = ::mmap (nullptr, mappingBytes, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    if (mapping == MAP_FAILED)
    {
        mapping = nullptr;
        closeRing();
        return;
    }
    // Touch every page now so the audio thread never takes a page fault
    // that has to allocate.
    std::memset (mapping, 0, mappingBytes);

    auto* base = static_cast<uint8_t*> (mapping);
    header = new (base) rslink::Header {};
    meta = reinterpret_cast<rslink::BlockMeta*> (base + rslink::kHeaderBytes);
    audio = reinterpret_cast<float*> (base + rslink::kHeaderBytes + rslink::kMetaSlots * rslink::kMetaBytes);
    std::memcpy (header->magic, rslink::kMagic, sizeof rslink::kMagic);
    header->version = rslink::kVersion;
    header->channels = channels;
    header->sampleRate = sampleRate;
    header->capacityFrames = capacityFrames;
    header->metaSlots = rslink::kMetaSlots;
    header->pid = (uint32_t) getpid();
    trackName.copyToUTF8 (header->trackName, sizeof header->trackName);
    juce::PluginHostType host;
    juce::String (host.getHostDescription()).copyToUTF8 (header->hostName, sizeof header->hostName);
    streamFrame = 0;
    header->state.store (rslink::kStateActive, std::memory_order_release);
}

void LinkProcessor::closeRing()
{
    if (header != nullptr)
        header->state.store (rslink::kStateClosed, std::memory_order_release);
    if (mapping != nullptr)
        ::munmap (mapping, mappingBytes);
    if (fd >= 0)
        ::close (fd);
    mapping = nullptr;
    header = nullptr;
    meta = nullptr;
    audio = nullptr;
    fd = -1;
}

void LinkProcessor::updateTrackProperties (const TrackProperties& props)
{
    trackName = props.name.value_or (juce::String());
    if (header != nullptr)
        trackName.copyToUTF8 (header->trackName, sizeof header->trackName);
}

void LinkProcessor::processBlock (juce::AudioBuffer<float>& buffer, juce::MidiBuffer&)
{
    // Pass-through: the buffer is left untouched.
    if (header == nullptr)
        return;
    const auto n = (uint64_t) buffer.getNumSamples();
    if (n == 0 || buffer.getNumChannels() < (int) channels)
        return;

    rslink::BlockMeta m {};
    m.frame = streamFrame;
    m.frames = (uint32_t) n;
    m.sampleRate = rate;
    m.hostMicros = (uint64_t) ((double) mach_absolute_time() * ticksToMicros);
    if (auto* ph = getPlayHead())
    {
        if (auto pos = ph->getPosition())
        {
            if (auto t = pos->getTimeInSamples())
            {
                m.timelineSample = *t;
                m.flags |= rslink::kHasTimeline;
            }
            if (pos->getIsPlaying())
                m.flags |= rslink::kPlaying;
            if (pos->getIsRecording())
                m.flags |= rslink::kRecording;
        }
    }

    // Audio, interleaved, wrapping at the ring end.
    for (uint64_t f = 0; f < n; ++f)
    {
        float* dst = audio + ((streamFrame + f) % capacityFrames) * channels;
        for (uint32_t c = 0; c < channels; ++c)
            dst[c] = buffer.getSample ((int) c, (int) f);
    }
    const auto block = header->writeBlocks.load (std::memory_order_relaxed);
    meta[block % rslink::kMetaSlots] = m;
    streamFrame += n;
    header->writeFrames.store (streamFrame, std::memory_order_release);
    header->writeBlocks.store (block + 1, std::memory_order_release);
}

juce::AudioProcessor* JUCE_CALLTYPE createPluginFilter() { return new LinkProcessor(); }
