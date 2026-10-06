package oikoarlo

import (
	"context"
	"slices"
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

// notice records a recording about to be announced, keeping its URLs for
// RecordingMedia unless Library listed it already; false if it was announced
// already.
func (b *Bridge) notice(r arlo.Recording) bool {
	key := recordingKey{r.CameraID, recordingID(r)}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, done := b.announced[key]; done {
		return false
	}
	if b.announced == nil {
		b.announced = map[recordingKey]time.Time{}
	}
	for k, start := range b.announced {
		if time.Since(start) > time.Hour { // long past a repeat or a backfill
			delete(b.announced, k)
		}
	}
	b.announced[key] = r.Created
	if b.listed == nil {
		b.listed = map[recordingKey]listedRecording{}
	}
	if _, ok := b.listed[key]; !ok {
		b.listed[key] = listedRecording{Recording: r, at: time.Now(), noticed: true}
	}
	return true
}

// announce reports a noticed recording's Event with its trigger, read from
// Library once it lists the recording, else as "other".
func (b *Bridge) announce(ctx context.Context, r arlo.Recording) {
	var found arlo.Recording
	for _, wait := range recheck {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		recs, _ := b.list(ctx, r.Created, r.Created) // a failure is a later try
		if i := slices.IndexFunc(recs, func(l arlo.Recording) bool {
			return l.CameraID == r.CameraID && l.Created.Equal(r.Created)
		}); i >= 0 {
			found = recs[i]
			break
		}
	}
	b.report(r.CameraID, r.Created, trigger(found))
}

// backfill announces the recent recordings a disconnection missed.
func (b *Bridge) backfill(ctx context.Context) {
	now := time.Now()
	recs, err := b.list(ctx, now.Add(-backfillWindow), now)
	if err != nil {
		b.log.Warn("recordings missed while disconnected not announced", "err", err)
		return
	}
	for _, r := range recs {
		if now.Sub(r.Created) <= backfillWindow && b.notice(r) {
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
