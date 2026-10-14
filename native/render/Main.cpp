// rs-render: renders audio through a chain of AU/VST3 plugins offline.
//
//   rs-render --rate 48000 --channels 2 [--block 512] [--timeline N]
//             [--tail FRAMES] --plugin <path-or-id> [--state FILE] ...
//   rs-render --list <path-or-id>
//
// Reads interleaved float32 from stdin and writes interleaved float32 to
// stdout. The chain's total reported latency is removed: output frame i is
// the chain's response to input frame i. --tail appends that many frames
// of response after the input ends (reverb/delay decay).
//
// Plugins run with isNonRealtime set and see a playing transport whose
// position is --timeline + frame, so tempo/position-aware plugins behave
// as they would in the session.
//
// stderr ends with one JSON line: {"latency":N,"plugins":[{"name":..,"latency":..}],"frames":N}
#include <juce_audio_processors/juce_audio_processors.h>
#include <juce_audio_utils/juce_audio_utils.h>

#include <cstdio>
#include <iostream>
#include <unistd.h>

namespace
{
struct Playhead final : juce::AudioPlayHead
{
    juce::int64 position = 0;
    double rate = 48000;

    juce::Optional<PositionInfo> getPosition() const override
    {
        PositionInfo p;
        p.setTimeInSamples (position);
        p.setTimeInSeconds ((double) position / rate);
        p.setIsPlaying (true);
        p.setIsRecording (false);
        return p;
    }
};

[[noreturn]] void fail (const juce::String& message)
{
    std::cerr << "rs-render: " << message << std::endl;
    std::exit (1);
}

juce::String jsonString (const juce::String& s)
{
    return juce::JSON::toString (juce::var (s));
}

std::unique_ptr<juce::AudioPluginInstance> load (juce::AudioPluginFormatManager& formats,
                                                 const juce::String& fileOrId,
                                                 double rate, int block)
{
    juce::OwnedArray<juce::PluginDescription> found;
    for (auto* format : formats.getFormats())
    {
        if (! format->fileMightContainThisPluginType (fileOrId))
            continue;
        format->findAllTypesForFile (found, fileOrId);
        if (! found.isEmpty())
            break;
    }
    if (found.isEmpty())
        fail ("no AU or VST3 plugin found at " + fileOrId);

    juce::String error;
    auto instance = formats.createPluginInstance (*found[0], rate, block, error);
    if (instance == nullptr)
        fail ("cannot load " + fileOrId + ": " + error);
    return instance;
}

void writeFully (const float* src, size_t count)
{
    auto* p = reinterpret_cast<const char*> (src);
    size_t want = count * sizeof (float);
    while (want > 0)
    {
        auto n = ::write (1, p, want);
        if (n <= 0)
            fail ("stdout closed");
        p += n;
        want -= (size_t) n;
    }
}
} // namespace

int main (int argc, char* argv[])
{
    juce::ScopedJuceInitialiser_GUI juce;
    juce::AudioPluginFormatManager formats;
    formats.addDefaultFormats();

    double rate = 0;
    int channels = 0, block = 512;
    juce::int64 timeline = 0, tail = 0;
    juce::StringArray pluginIds, stateFiles;
    juce::String listId;

    for (int i = 1; i < argc; ++i)
    {
        juce::String a (argv[i]);
        auto next = [&] { if (i + 1 >= argc) fail ("missing value for " + a); return juce::String (argv[++i]); };
        if (a == "--rate") rate = next().getDoubleValue();
        else if (a == "--channels") channels = next().getIntValue();
        else if (a == "--block") block = next().getIntValue();
        else if (a == "--timeline") timeline = next().getLargeIntValue();
        else if (a == "--tail") tail = next().getLargeIntValue();
        else if (a == "--plugin") { pluginIds.add (next()); stateFiles.add ({}); }
        else if (a == "--state")
        {
            if (stateFiles.isEmpty()) fail ("--state must follow a --plugin");
            stateFiles.set (stateFiles.size() - 1, next());
        }
        else if (a == "--list") listId = next();
        else fail ("unknown argument " + a);
    }

    if (listId.isNotEmpty())
    {
        auto p = load (formats, listId, 48000, 512);
        std::cout << "{\"name\":" << jsonString (p->getName())
                  << ",\"format\":" << jsonString (p->getPluginDescription().pluginFormatName)
                  << ",\"inputs\":" << p->getTotalNumInputChannels()
                  << ",\"outputs\":" << p->getTotalNumOutputChannels() << "}" << std::endl;
        return 0;
    }
    if (rate <= 0 || channels <= 0 || block <= 0 || pluginIds.isEmpty())
        fail ("usage: rs-render --rate R --channels C [--block N] [--timeline N] [--tail N] --plugin ID [--state FILE] ...");

    Playhead playhead;
    playhead.rate = rate;
    std::vector<std::unique_ptr<juce::AudioPluginInstance>> chain;
    int latency = 0;
    juce::String report;
    for (int i = 0; i < pluginIds.size(); ++i)
    {
        auto p = load (formats, pluginIds[i], rate, block);
        // Same channel count in and out on the main bus; no sidechain.
        auto layout = p->getBusesLayout();
        for (auto* buses : { &layout.inputBuses, &layout.outputBuses })
            for (int b = 0; b < buses->size(); ++b)
                buses->getReference (b) = b == 0 ? juce::AudioChannelSet::canonicalChannelSet (channels)
                                                 : juce::AudioChannelSet::disabled();
        if (! p->setBusesLayout (layout))
            fail (p->getName() + " does not support " + juce::String (channels) + " channels in and out");
        if (stateFiles[i].isNotEmpty())
        {
            juce::MemoryBlock state;
            if (! juce::File (stateFiles[i]).loadFileAsData (state))
                fail ("cannot read " + stateFiles[i]);
            p->setStateInformation (state.getData(), (int) state.getSize());
        }
        p->setNonRealtime (true);
        p->setPlayHead (&playhead);
        p->prepareToPlay (rate, block);
        // Latency is only final after prepareToPlay.
        auto l = p->getLatencySamples();
        latency += l;
        report << (i ? "," : "") << "{\"name\":" << jsonString (p->getName())
               << ",\"format\":" << jsonString (p->getPluginDescription().pluginFormatName)
               << ",\"latency\":" << l << "}";
        chain.push_back (std::move (p));
    }

    int bufferChannels = channels;
    for (auto& p : chain)
        bufferChannels = juce::jmax (bufferChannels, p->getTotalNumInputChannels(), p->getTotalNumOutputChannels());
    juce::AudioBuffer<float> buffer (bufferChannels, block);
    juce::MidiBuffer midi;
    std::vector<float> interleaved ((size_t) block * (size_t) channels);

    // Chain output frame j is the response to input frame j - latency.
    juce::int64 inFrames = 0, fed = 0, emitted = 0;
    bool eof = false;
    for (;;)
    {
        int n = 0;
        if (! eof)
        {
            size_t got = 0;
            auto* p = reinterpret_cast<char*> (interleaved.data());
            const size_t want = interleaved.size() * sizeof (float);
            while (got < want)
            {
                auto r = ::read (0, p + got, want - got);
                if (r <= 0) { eof = true; break; }
                got += (size_t) r;
            }
            n = (int) (got / (sizeof (float) * (size_t) channels));
            inFrames += n;
        }
        std::fill (interleaved.begin() + (long) n * channels, interleaved.end(), 0.0f);
        buffer.clear();
        for (int f = 0; f < n; ++f)
            for (int c = 0; c < channels; ++c)
                buffer.setSample (c, f, interleaved[(size_t) f * channels + c]);

        playhead.position = timeline + fed;
        for (auto& p : chain)
            p->processBlock (buffer, midi);
        midi.clear();
        fed += block;

        // This block's outputs answer input frames [fed - block - latency, fed - latency).
        const juce::int64 first = fed - block - latency;
        juce::int64 lo = juce::jmax (first, emitted);
        juce::int64 hi = fed - latency;
        if (eof)
            hi = juce::jmin (hi, inFrames + tail);
        if (hi > lo)
        {
            for (juce::int64 f = lo; f < hi; ++f)
                for (int c = 0; c < channels; ++c)
                    interleaved[(size_t) (f - lo) * channels + c] = buffer.getSample (c, (int) (f - first));
            writeFully (interleaved.data(), (size_t) (hi - lo) * channels);
            emitted = hi;
        }
        if (eof && emitted >= inFrames + tail)
            break;
    }
    for (auto& p : chain)
        p->releaseResources();
    std::cerr << "{\"latency\":" << latency << ",\"plugins\":[" << report << "],\"frames\":" << emitted << "}" << std::endl;
    return 0;
}
