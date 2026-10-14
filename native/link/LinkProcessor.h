#pragma once

#include <juce_audio_processors/juce_audio_processors.h>

#include "LinkShared.h"

// RemoteSession Link: a zero-latency pass-through insert that publishes the
// track's audio, stamped per block with the host's timeline position and
// transport state, to the RemoteSession middle layer.
class LinkProcessor final : public juce::AudioProcessor
{
public:
    LinkProcessor();
    ~LinkProcessor() override;

    void prepareToPlay (double sampleRate, int maximumExpectedSamplesPerBlock) override;
    void releaseResources() override;
    bool isBusesLayoutSupported (const BusesLayout&) const override;
    void processBlock (juce::AudioBuffer<float>&, juce::MidiBuffer&) override;
    void processBlock (juce::AudioBuffer<double>&, juce::MidiBuffer&) override {}

    juce::AudioProcessorEditor* createEditor() override { return nullptr; }
    bool hasEditor() const override { return false; }
    const juce::String getName() const override { return "RemoteSession Link"; }
    bool acceptsMidi() const override { return false; }
    bool producesMidi() const override { return false; }
    double getTailLengthSeconds() const override { return 0; }
    int getNumPrograms() override { return 1; }
    int getCurrentProgram() override { return 0; }
    void setCurrentProgram (int) override {}
    const juce::String getProgramName (int) override { return {}; }
    void changeProgramName (int, const juce::String&) override {}
    void getStateInformation (juce::MemoryBlock&) override {}
    void setStateInformation (const void*, int) override {}
    void updateTrackProperties (const TrackProperties&) override;

    // Where link files live; RS_LINK_DIR overrides (tests).
    static juce::File linksDirectory();

private:
    void openRing (double sampleRate, int channels);
    void closeRing();

    int fd = -1;
    void* mapping = nullptr;
    size_t mappingBytes = 0;
    juce::File ringFile;
    rslink::Header* header = nullptr;
    rslink::BlockMeta* meta = nullptr;
    float* audio = nullptr;
    uint64_t capacityFrames = 0;
    uint32_t channels = 0;
    double rate = 0;
    uint64_t streamFrame = 0;
    double ticksToMicros = 0;
    juce::String trackName;

    JUCE_DECLARE_NON_COPYABLE_WITH_LEAK_DETECTOR (LinkProcessor)
};
