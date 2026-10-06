package oikoarlo

import (
	"context"
	"time"

	arlo "github.com/llehouerou/go-arlo"

	"github.com/llehouerou/oiko/bridge"
)

// triggers are the data of a recording Event: what trigger names, and
// "other" when it names nothing.
var triggers = []string{"motion", "person", "vehicle", "animal", "package", "sound", "other"}

// recheck is how long announce waits before each look for a noticed
// recording in Library, which lists it some seconds after its notice.
var recheck = []time.Duration{5 * time.Second, 10 * time.Second, 30 * time.Second}

// backfillWindow is how far back a reconnection announces the recordings its
// disconnection missed: older ones would be stale news.
const backfillWindow = 15 * time.Minute

// announce reports a noticed recording's Event with its trigger, read from
// the Library once it lists the recording, else as "other".
func (b *Bridge) announce(ctx context.Context, r arlo.Recording) {
	var found arlo.Recording
	for _, wait := range recheck {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if l, ok := b.lib.listed(ctx, r.CameraID, r.Created); ok {
			found = l
			break
		}
	}
	b.report(r.CameraID, r.Created, trigger(found))
}

// backfill announces the recent recordings a disconnection missed.
func (b *Bridge) backfill(ctx context.Context) {
	now := time.Now()
	recs, err := b.lib.videos(ctx, now.Add(-backfillWindow), now)
	if err != nil {
		b.log.Warn("recordings missed while disconnected not announced", "err", err)
		return
	}
	for _, r := range recs {
		if b.lib.notice(r) {
			b.report(r.CameraID, r.Created, trigger(r))
		}
	}
}

// report is a recording's Event, as of its start, by which Oiko finds it.
func (b *Bridge) report(cameraID string, start time.Time, trigger string) {
	if trigger == "" {
		trigger = "other"
	}
	b.port.Report(cameraID, []bridge.Reading{{Function: camera, Capability: bridge.RecordingEvent, Data: trigger}}, start)
}
