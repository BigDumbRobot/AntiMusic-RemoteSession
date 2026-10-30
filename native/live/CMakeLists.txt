cmake_minimum_required(VERSION 3.22)
# One architecture per build tree (Opus probes CPU features at configure
# time); the Makefile builds arm64 and x86_64 and joins them with lipo.
set(CMAKE_OSX_DEPLOYMENT_TARGET "11.0" CACHE STRING "Minimum macOS version")
project(RemoteSessionLive LANGUAGES C)

include(FetchContent)
FetchContent_Declare(opus
    URL https://downloads.xiph.org/releases/opus/opus-1.5.2.tar.gz
    URL_HASH SHA256=65c1d2f78b9f2fb20082c38cbe47c951ad5839345876e46941612ee87f9a7ce1
    DOWNLOAD_EXTRACT_TIMESTAMP ON)
set(OPUS_BUILD_SHARED_LIBRARY OFF CACHE BOOL "" FORCE)
set(OPUS_BUILD_TESTING OFF CACHE BOOL "" FORCE)
set(OPUS_BUILD_PROGRAMS OFF CACHE BOOL "" FORCE)
set(OPUS_INSTALL_PKG_CONFIG_MODULE OFF CACHE BOOL "" FORCE)
set(OPUS_INSTALL_CMAKE_CONFIG_MODULE OFF CACHE BOOL "" FORCE)
FetchContent_MakeAvailable(opus)

add_executable(rs-live rs-live.c)
target_compile_options(rs-live PRIVATE -O2 -Wall -Wextra)
target_link_libraries(rs-live PRIVATE opus "-framework CoreAudio" "-framework CoreFoundation")
