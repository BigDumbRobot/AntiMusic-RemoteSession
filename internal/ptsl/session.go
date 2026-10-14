package ptsl

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"antimusic.dev/remotesession/internal/timeline"
)

// SessionInfo is the session metadata needed to place recordings.
type SessionInfo struct {
	Name          string             `json:"name"`
	SampleRate    int                `json:"sample_rate"`
	StartTimecode string             `json:"start_timecode"`
	TimecodeRate  string             `json:"timecode_rate"`
	FrameRate     timeline.FrameRate `json:"-"`
	// StartSample is the session start in samples since 00:00:00:00.
	StartSample int64 `json:"start_sample"`
}

// PTSL enums declare several aliases per value (e.g. "TS_TransportPlaying"
// and "TState_TransportPlaying"; "SSTCRate_Fps2997Drop" and
// "STCRate_Fps2997Drop"), and protobuf JSON emits whichever is declared
// first. Values are therefore matched on the text after the first "_",
// which every alias of a value shares.
func enumSuffix(s string) string {
	if i := strings.IndexByte(s, '_'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// Keyed by enumSuffix of ptsl.SessionTimeCodeRate names.
var timecodeRates = map[string]timeline.FrameRate{
	"Fps23976":    timeline.FPS23976,
	"Fps24":       timeline.FPS24,
	"Fps25":       timeline.FPS25,
	"Fps2997":     timeline.FPS2997,
	"Fps2997Drop": timeline.FPS2997DF,
	"Fps30":       timeline.FPS30,
	"Fps50":       timeline.FPS50,
	"Fps5994":     timeline.FPS5994,
	"Fps5994Drop": timeline.FPS5994DF,
	"Fps60":       timeline.FPS60,
}

// Session reads the open session's name, sample rate and start time.
func (c *Client) Session(ctx context.Context) (SessionInfo, error) {
	var s SessionInfo
	var name struct {
		SessionName string `json:"session_name"`
	}
	if err := c.Call(ctx, CmdGetSessionName, nil, &name); err != nil {
		return s, err
	}
	s.Name = name.SessionName
	var sr struct {
		SampleRate string `json:"sample_rate"`
	}
	if err := c.Call(ctx, CmdGetSessionSampleRate, nil, &sr); err != nil {
		return s, err
	}
	n, err := strconv.Atoi(enumSuffix(sr.SampleRate))
	if err != nil {
		return s, fmt.Errorf("ptsl: unexpected sample rate %q", sr.SampleRate)
	}
	s.SampleRate = n
	var tcr struct {
		CurrentSetting string `json:"current_setting"`
	}
	if err := c.Call(ctx, CmdGetSessionTimeCodeRate, nil, &tcr); err != nil {
		return s, err
	}
	s.TimecodeRate = tcr.CurrentSetting
	fr, ok := timecodeRates[enumSuffix(tcr.CurrentSetting)]
	if !ok {
		return s, fmt.Errorf("ptsl: unsupported timecode rate %q", tcr.CurrentSetting)
	}
	s.FrameRate = fr
	var st struct {
		SessionStartTime string `json:"session_start_time"`
	}
	if err := c.Call(ctx, CmdGetSessionStartTime, nil, &st); err != nil {
		return s, err
	}
	s.StartTimecode = st.SessionStartTime
	s.StartSample, err = timeline.TimecodeToSamples(st.SessionStartTime, fr, n)
	return s, err
}

// TransportState returns the current ptsl.TransportState name, e.g.
// "TS_TransportPlaying".
func (c *Client) TransportState(ctx context.Context) (string, error) {
	var r struct {
		CurrentSetting string `json:"current_setting"`
	}
	err := c.Call(ctx, CmdGetTransportState, nil, &r)
	return r.CurrentSetting, err
}

// Rolling reports whether a transport state plays audio at normal speed.
// Half-speed and shuttle states are deliberately excluded: their timeline
// mapping is not linear at the session rate.
func Rolling(state string) bool {
	s := enumSuffix(state)
	return s == "TransportPlaying" || s == "TransportRecording"
}

// Selection is the timeline selection in samples.
type Selection struct {
	InSample, OutSample   int64
	PreRollStartSample    int64
	PreRollEnabled        bool
	PlayStartMarkerSample int64
}

// TimelineSelection reads the timeline selection with positions in
// samples.
func (c *Client) TimelineSelection(ctx context.Context) (Selection, error) {
	var r struct {
		PlayStartMarkerTime string `json:"play_start_marker_time"`
		InTime              string `json:"in_time"`
		OutTime             string `json:"out_time"`
		PreRollStartTime    string `json:"pre_roll_start_time"`
		PreRollEnabled      bool   `json:"pre_roll_enabled"`
	}
	body := map[string]string{"time_scale": "Samples"}
	if err := c.Call(ctx, CmdGetTimelineSelection, body, &r); err != nil {
		return Selection{}, err
	}
	var s Selection
	var err error
	parse := func(v string) int64 {
		if err != nil || v == "" {
			return 0
		}
		var n int64
		n, err = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	}
	s.InSample = parse(r.InTime)
	s.OutSample = parse(r.OutTime)
	s.PreRollStartSample = parse(r.PreRollStartTime)
	s.PlayStartMarkerSample = parse(r.PlayStartMarkerTime)
	s.PreRollEnabled = r.PreRollEnabled
	return s, err
}

// SystemDelay returns the session's delay-compensation total in samples.
func (c *Client) SystemDelay(ctx context.Context) (samples int32, enabled bool, err error) {
	var r struct {
		Samples                  int32 `json:"samples"`
		DelayCompensationEnabled bool  `json:"delay_compensation_enabled"`
	}
	err = c.Call(ctx, CmdGetSessionSystemDelay, nil, &r)
	return r.Samples, r.DelayCompensationEnabled, err
}

// ImportSpotted imports audio files onto new tracks with their start
// spotted at a sample position given in the session's sample counter
// units.
func (c *Client) ImportSpotted(ctx context.Context, files []string, destDir string, sample int64) error {
	body := map[string]any{
		"import_type": "Audio",
		"audio_data": map[string]any{
			"file_list":        files,
			"audio_operations": "CopyAudio",
			"destination_path": destDir,
			"destination":      "MD_NewTrack",
			"location":         "ML_Spot",
			"location_data": map[string]any{
				"location_type":    "Start",
				"location_options": "Samples",
				"location_value":   strconv.FormatInt(sample, 10),
			},
		},
	}
	return c.Call(ctx, CmdImport, body, nil)
}
