cmake_minimum_required(VERSION 3.22)

set(CMAKE_OSX_DEPLOYMENT_TARGET "11.0" CACHE STRING "Minimum macOS version")
# Universal by default: Pro Tools may run natively or under Rosetta.
set(CMAKE_OSX_ARCHITECTURES "arm64;x86_64" CACHE STRING "Target architectures")

project(RemoteSessionNative VERSION 0.1.0 LANGUAGES C CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

# Same JUCE as Anti-Music's Panner. JUCE 8 bundles the AAX SDK. Set
# FETCHCONTENT_SOURCE_DIR_JUCE to an existing checkout to build offline.
include(FetchContent)
FetchContent_Declare(JUCE
    URL https://github.com/juce-framework/JUCE/archive/refs/tags/8.0.10.tar.gz
    DOWNLOAD_EXTRACT_TIMESTAMP ON)
FetchContent_MakeAvailable(JUCE)

# rs-render: offline plugin-chain renderer for post-processing prints.
juce_add_console_app(rs-render PRODUCT_NAME "rs-render")
target_sources(rs-render PRIVATE render/Main.cpp)
target_compile_definitions(rs-render PRIVATE
    JUCE_PLUGINHOST_AU=1
    JUCE_PLUGINHOST_VST3=1
    JUCE_USE_CURL=0
    JUCE_WEB_BROWSER=0)
target_link_libraries(rs-render PRIVATE
    juce::juce_audio_processors
    juce::juce_audio_utils
    juce::juce_recommended_config_flags
    juce::juce_recommended_warning_flags)

# RemoteSession Link: pass-through insert that publishes timeline-stamped
# audio to the middle layer. Universal binary: Pro Tools 2026.4 runs as an
# Intel process under Rosetta on Apple Silicon. The AAX build is unsigned
# and loads only in Pro Tools Developer builds until signed with PACE tools.
set(LINK_FORMATS VST3 AAX)
if(APPLE)
    list(APPEND LINK_FORMATS AU)
endif()
juce_add_plugin(RemoteSessionLink
    PRODUCT_NAME "RemoteSession Link"
    COMPANY_NAME "Anti-Music"
    BUNDLE_ID com.antimusic.remotesession.link
    PLUGIN_MANUFACTURER_CODE Antm
    PLUGIN_CODE RsLk
    FORMATS ${LINK_FORMATS}
    IS_SYNTH FALSE
    NEEDS_MIDI_INPUT FALSE
    NEEDS_MIDI_OUTPUT FALSE
    VST3_CATEGORIES Fx Tools
    AAX_CATEGORY None
    AU_MAIN_TYPE kAudioUnitType_Effect
    COPY_PLUGIN_AFTER_BUILD FALSE)
target_sources(RemoteSessionLink PRIVATE link/LinkProcessor.cpp)
target_compile_definitions(RemoteSessionLink PUBLIC
    JUCE_WEB_BROWSER=0
    JUCE_USE_CURL=0
    JUCE_VST3_CAN_REPLACE_VST2=0
    JUCE_DISPLAY_SPLASH_SCREEN=0)
target_link_libraries(RemoteSessionLink PRIVATE
    juce::juce_audio_utils
    juce::juce_recommended_config_flags
    juce::juce_recommended_lto_flags
    juce::juce_recommended_warning_flags)
