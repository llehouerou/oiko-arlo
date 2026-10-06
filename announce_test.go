package oikoarlo

import (
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	arlo "github.com/llehouerou/go-arlo"

	"github.com/llehouerou/oiko/bridge"
	"github.com/llehouerou/oiko/bridge/bridgetest"
)

// lagging is a library that lists its recordings from listedAt on, and
// counts its calls.
type lagging struct {
	mu       sync.Mutex
	calls    int
	listedAt time.Time
	recs     []arlo.Recording
}

func (l *lagging) list(from, to time.Time) []arlo.Recording {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if time.Now().Before(l.listedAt) {
		return nil
	}
	return slices.Clone(l.recs)
}

func (l *lagging) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// video is a recording of cam as its notice tells it: no Duration, Reason
// or Object.
func video(cam string, created time.Time) arlo.Recording {
	return arlo.Recording{CameraID: cam, Created: created, ContentType: "video/mp4",
		URL: "https://s3.example.com/" + cam + ".mp4", ThumbnailURL: "https://s3.example.com/" + cam + ".jpg"}
}

func event(t *testing.T, h *bridgetest.Home, cam string) (bridgetest.Value, bool) {
	t.Helper()
	return h.Event(cam, "camera", bridge.RecordingEvent)
}

// TestAnnounce notices a recording Library lists 12 s later: Recordings has
// it at once, and its Event, as of its start, carries Library's trigger.
func TestAnnounce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now().Add(-30 * time.Second).Truncate(time.Millisecond)
		listed := video("CAM1", start)
		listed.Object, listed.Duration = "Person", 20*time.Second
		l := &lagging{listedAt: time.Now().Add(12 * time.Second), recs: []arlo.Recording{listed}}
		b, h := newHome(fakeClient{library: l.list})

		b.handle(arlo.RecordingAdded{Recording: video("CAM1", start)})
		if rs, err := h.Recordings("CAM1", "camera", start.Add(-5*time.Second), start.Add(5*time.Second)); err != nil || len(rs) != 1 || !rs[0].Start.Equal(start) {
			t.Errorf("recordings before Library lists it: %+v, %v", rs, err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if e, ok := event(t, h, "CAM1"); !ok || e.Data != "person" || !e.At.Equal(start) {
			t.Errorf("event = %+v, %v; want person at %s", e, ok, start)
		}
		if n := l.count(); n != 3 { // Recordings', then at 5 s and 15 s
			t.Errorf("%d Library calls, want 3", n)
		}
		if rs, _ := h.Recordings("CAM1", "camera", start, start); len(rs) != 1 || rs[0].Duration != 20*time.Second {
			t.Errorf("recordings once listed: %+v", rs)
		}
	})
}

// TestAnnounceUnlisted notices a recording Library never lists, twice: one
// Event, "other", after three looks.
func TestAnnounceUnlisted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now().Add(-30 * time.Second)
		l := &lagging{}
		b, h := newHome(fakeClient{library: l.list})
		b.handle(arlo.RecordingAdded{Recording: video("CAM1", start)})
		b.handle(arlo.RecordingAdded{Recording: video("CAM1", start)})
		b.handle(arlo.RecordingAdded{Recording: arlo.Recording{CameraID: "CAM2", Created: start, ContentType: "image/jpg"}})
		time.Sleep(time.Minute)
		synctest.Wait()
		if e, ok := event(t, h, "CAM1"); !ok || e.Data != "other" {
			t.Errorf("event = %+v, %v; want other", e, ok)
		}
		if n := l.count(); n != 3 {
			t.Errorf("%d Library calls, want 3: one announcement", n)
		}
		if _, ok := event(t, h, "CAM2"); ok {
			t.Error("a snapshot announced")
		}
	})
}

// TestAnnounceAgain notices a recording once more, over an hour after its
// start: it is announced anew.
func TestAnnounceAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := video("CAM1", time.Now().Add(-30*time.Second))
		l := &lagging{}
		b, _ := newHome(fakeClient{library: l.list})
		b.handle(arlo.RecordingAdded{Recording: r})
		time.Sleep(time.Hour)
		b.handle(arlo.RecordingAdded{Recording: r})
		time.Sleep(time.Minute)
		synctest.Wait()
		if n := l.count(); n != 6 {
			t.Errorf("%d Library calls, want 6: two announcements", n)
		}
	})
}

// TestBackfill reconnects: recordings of the last 15 minutes not announced
// are; older ones, and those on the first connection, are not.
func TestBackfill(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		recent, old := video("CAM1", now.Add(-10*time.Minute)), video("CAM2", now.Add(-20*time.Minute))
		recent.Reason = "motionRecord"
		l := &lagging{recs: []arlo.Recording{recent, old}}
		b, h := newHome(fakeClient{library: l.list})
		synctest.Wait()
		if n := l.count(); n != 0 {
			t.Errorf("first connection: %d Library calls", n)
		}
		b.handle(arlo.Connection{Up: false})
		b.handle(arlo.Connection{Up: true})
		synctest.Wait()
		if e, ok := event(t, h, "CAM1"); !ok || e.Data != "motion" || !e.At.Equal(recent.Created) {
			t.Errorf("recent: %+v, %v", e, ok)
		}
		if e, ok := event(t, h, "CAM2"); ok {
			t.Errorf("older than 15 minutes announced: %+v", e)
		}
		b.handle(arlo.RecordingAdded{Recording: recent}) // late notice
		time.Sleep(time.Minute)
		synctest.Wait()
		if n := l.count(); n != 1 {
			t.Errorf("%d Library calls, want 1: the backfill", n)
		}
	})
}
