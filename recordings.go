package oikoarlo

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	arlo "github.com/llehouerou/go-arlo"

	"github.com/llehouerou/oiko/bridge"
)

var _ bridge.Recordings = (*Bridge)(nil)

// retention is how far back Arlo keeps recordings, at most.
const retention = 31 * 24 * time.Hour

// Recordings lists a camera's videos, from Arlo's Library. The ID of a
// recording is its start in Unix milliseconds.
func (b *Bridge) Recordings(ctx context.Context, address, _ string, from, to time.Time) ([]bridge.Recording, error) {
	if oldest := time.Now().Add(-retention); from.Before(oldest) {
		from = oldest // Oiko's "All" asks from 1970
	}
	if to.Before(from) {
		return nil, nil
	}
	recs, err := b.lib.videos(ctx, from, to)
	if err != nil {
		return nil, fmt.Errorf("arlo: %w", err)
	}
	var out []bridge.Recording
	for _, r := range recs {
		if r.CameraID == address {
			out = append(out, bridge.Recording{ID: recordingID(r), Start: r.Created, Duration: r.Duration, Trigger: trigger(r)})
		}
	}
	slices.SortFunc(out, func(a, b bridge.Recording) int { return b.Start.Compare(a.Start) })
	return out, nil
}

// RecordingMedia GETs a recording's video or thumbnail with header.
func (b *Bridge) RecordingMedia(ctx context.Context, address, _, id string, part bridge.RecordingPart, header http.Header) (*http.Response, error) {
	ms, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("arlo: recording %s of %s: %w", id, address, bridge.ErrNotFound)
	}
	resp, err := b.lib.media(ctx, address, time.UnixMilli(ms), part, header)
	if err != nil {
		return nil, fmt.Errorf("arlo: %s of recording %s of %s: %w", part, id, address, err)
	}
	return resp, nil
}

func recordingID(r arlo.Recording) string { return strconv.FormatInt(r.Created.UnixMilli(), 10) }

// trigger is what the camera saw, else why it recorded.
func trigger(r arlo.Recording) string {
	reason := strings.ToLower(r.Reason)
	switch {
	case r.Object != "":
		return strings.ToLower(r.Object)
	case strings.Contains(reason, "motion"):
		return "motion"
	case strings.Contains(reason, "audio"), strings.Contains(reason, "sound"):
		return "sound"
	}
	return ""
}
