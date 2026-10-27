# Testing the Link plugin in Pro Tools

Answers the open questions in [ARCHITECTURE.md](ARCHITECTURE.md) section 7:
position under delay compensation, behaviour while stopped, track names.
Needs the Pro Tools Developer build with its licence in iLok.

## Setup

1. `make` (the plugin builds as a universal binary).
2. Install the AAX:
   ```sh
   sudo cp -R "native/build/RemoteSessionLink_artefacts/Release/AAX/RemoteSession Link.aaxplugin" \
     "/Library/Application Support/Avid/Audio/Plug-Ins/"
   ```
3. Open the Developer build. New session, 48 kHz. Set the main counter to
   **Samples**.

## 1. Loads and reports the track name

1. New stereo audio track named `Click`. Insert **RemoteSession Link** on it
   (Insert > plug-in > Other or Utility).
2. `bin/rsession links`: expect `active  "Click"  Pro Tools ... 48000 Hz  2 ch`.
3. Rename the track to `Click2` and run `links` again.

Record: does the name appear? Does it follow the rename?

## 2. Stopped behaviour

`bin/rsession links -watch Click`, leave transport stopped for 10 s, then
click around the timeline while stopped.

Record: does it print blocks while stopped (`… N blocks`), or `no blocks`?
Does the position move when you click the timeline while stopped?

## 3. Baseline position

1. Put a short click (e.g. Signal Generator printed, or any sharp transient)
   on `Click` so the transient sits at a known sample, say 96000. Use the
   Grid/Spot mode to place it exactly.
2. With `-watch` running, play from before the click.

Expect `onset ... at timeline 96000`. Any constant offset here is Pro Tools'
position convention (e.g. counted from session start); note it, and
subtract it in the next step.

## 4. Delay compensation (the main question)

1. In insert slot A, before Link, add a plugin that reports latency (the Mix
   window's delay readout shows a non-zero `dly`; an oversampling or
   linear-phase Plugin Alliance plugin works). Note its latency L.
2. Delay Compensation **on** (Options menu). Play past the click.
3. Delay Compensation **off**. Play again.

Record both onset positions. If compensation is on and the onset is still at
96000, Pro Tools reports positions already compensated for upstream latency.
If it is at 96000 + L, the recorder has to subtract the latency.

## 5. End to end

1. `bin/rsession record -link Click -dir takes/pt1 -ptsl`, play through the
   click with a stop, a locate, and a loop, then Ctrl-C.
2. `bin/rsession deliver -dir takes/pt1`, then `bin/rsession ptsl-import -dir takes/pt1`.
3. Zoom in on the imported clips: the click must line up sample for sample
   with the original.

Write the results into ARCHITECTURE.md section 7 and remove the
"Unverified" note.
