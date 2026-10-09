package oikoarlo

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	arlo "github.com/llehouerou/go-arlo"

	"github.com/llehouerou/oiko/bridge"
	"github.com/llehouerou/oiko/bridge/bridgetest"
)

// fakeLibrary is a fake of Arlo's Library: its entries' URLs point at an S3
// stand-in that refuses a signature other than the latest listing's.
type fakeLibrary struct {
	s3      network
	base    time.Time // the start of CAM1's first recording in range
	calls   [][2]time.Time
	mu      sync.Mutex
	sig     string // the latest listing's signature
	nextSig string
}

func (l *fakeLibrary) signature() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sig
}

func newFakeLibrary() *fakeLibrary {
	l := &fakeLibrary{base: time.Now().Truncate(time.Hour).Add(-48 * time.Hour), nextSig: "s1"}
	l.s3 = network{"s3.test": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("X-Amz-Signature") != l.signature() {
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		http.ServeContent(w, r, r.URL.Path, l.base, bytes.NewReader([]byte("media of "+r.URL.Path)))
	})}
	return l
}

// list answers Library: CAM1's four videos and a snapshot in range, one
// video before it, and CAM2's video.
func (l *fakeLibrary) list(from, to time.Time) []arlo.Recording {
	l.calls = append(l.calls, [2]time.Time{from, to})
	l.mu.Lock()
	l.sig = l.nextSig
	l.mu.Unlock()
	rec := func(cam string, created time.Time, ct, reason, object string) arlo.Recording {
		u := "http://s3.test/" + cam + "/" + recordingID(arlo.Recording{Created: created})
		return arlo.Recording{CameraID: cam, Created: created, Duration: 20 * time.Second, ContentType: ct, Reason: reason, Object: object,
			URL: u + ".mp4?X-Amz-Signature=" + l.sig, ThumbnailURL: u + ".jpg?X-Amz-Signature=" + l.sig}
	}
	return []arlo.Recording{
		rec("CAM1", l.base.Add(-5*time.Hour), "video/mp4", "motionRecord", ""),
		rec("CAM1", l.base, "video/mp4", "motionRecord", "Vehicle"),
		rec("CAM1", l.base.Add(30*time.Minute), "image/jpg", "", ""),
		rec("CAM1", l.base.Add(time.Hour), "video/mp4", "motionRecord", ""),
		rec("CAM2", l.base.Add(time.Hour), "video/mp4", "motionRecord", ""),
		rec("CAM1", l.base.Add(90*time.Minute), "video/mp4", "userRecord", ""),
		rec("CAM1", l.base.Add(2*time.Hour), "video/mp4", "audioRecord", ""),
	}
}

func (l *fakeLibrary) home(t *testing.T) *bridgetest.Home {
	h, _ := home(t, fakeClient{library: l.list, s3: l.s3})
	return h
}

// TestRecordings lists CAM1's videos in range, the newest first, each with
// its trigger, from the days around it, and clamps a range from 1970 to
// Arlo's retention.
func TestRecordings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newFakeLibrary()
		h := l.home(t)
		rs, err := h.Recordings("CAM1", "camera", l.base, l.base.Add(2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range rs {
			got = append(got, r.Start.Sub(l.base).String()+" "+r.Trigger+" "+r.Duration.String())
			if r.ID != recordingID(arlo.Recording{Created: r.Start}) {
				t.Errorf("ID %s for %s", r.ID, r.Start)
			}
		}
		if want := []string{"2h0m0s sound 20s", "1h30m0s  20s", "1h0m0s motion 20s", "0s vehicle 20s"}; !slices.Equal(got, want) {
			t.Errorf("recordings = %q, want %q", got, want)
		}
		if c := l.calls[0]; c[0].After(l.base.Add(-23*time.Hour)) || c[1].Before(l.base.Add(25*time.Hour)) {
			t.Errorf("library listed %s to %s: not the days around", c[0], c[1])
		}

		if _, err := h.Recordings("CAM1", "camera", time.Unix(0, 0), time.Now()); err != nil {
			t.Fatal(err)
		}
		if from := l.calls[1][0]; from.Before(time.Now().Add(-33 * 24 * time.Hour)) {
			t.Errorf("library listed from %s, past Arlo's retention", from)
		}
	})
}

// TestRecordingMedia fetches a listed video's range, its thumbnail, an
// unlisted one, a refused one, and checks no error quotes a URL.
func TestRecordingMedia(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newFakeLibrary()
		h := l.home(t)
		rs, err := h.Recordings("CAM1", "camera", l.base, l.base)
		if err != nil || len(rs) != 1 {
			t.Fatalf("recordings: %+v, %v", rs, err)
		}
		id := rs[0].ID
		media := func(id string, part bridge.RecordingPart, header http.Header) (int, string, error) {
			resp, err := h.RecordingMedia("CAM1", "camera", id, part, header)
			if err != nil {
				return 0, "", err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(body), nil
		}

		// From the listing: no other Library call.
		if status, body, err := media(id, bridge.Video, http.Header{"Range": {"bytes=0-4"}}); err != nil || status != http.StatusPartialContent || body != "media" {
			t.Errorf("video range: %d %q %v", status, body, err)
		}
		if status, body, err := media(id, bridge.Thumbnail, nil); err != nil || status != http.StatusOK || !strings.HasSuffix(body, ".jpg") {
			t.Errorf("thumbnail: %d %q %v", status, body, err)
		}
		if len(l.calls) != 1 {
			t.Errorf("%d Library calls, want 1", len(l.calls))
		}

		// Its URL expired: listed again, then fetched.
		l.mu.Lock()
		l.sig, l.nextSig = "s2", "s2"
		l.mu.Unlock()
		if status, _, err := media(id, bridge.Video, nil); err != nil || status != http.StatusOK {
			t.Errorf("expired video: %d %v", status, err)
		}
		if len(l.calls) != 2 {
			t.Errorf("%d Library calls, want 2", len(l.calls))
		}

		// Not listed yet: its day is.
		other := recordingID(arlo.Recording{Created: l.base.Add(time.Hour)})
		resp, err := l.home(t).RecordingMedia("CAM1", "camera", other, bridge.Video, nil)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Errorf("unlisted video: %v", err)
		} else {
			resp.Body.Close()
		}
		if c := l.calls[2]; c[0].After(l.base) || c[1].Before(l.base.Add(time.Hour)) {
			t.Errorf("library listed %s to %s: not the recording's day", c[0], c[1])
		}

		// CAM2 recorded at that hour, not two hours after.
		for _, c := range [][2]string{{"CAM1", "123"}, {"CAM1", "abc"}, {"CAM2", recordingID(arlo.Recording{Created: l.base.Add(2 * time.Hour)})}} {
			if _, err := h.RecordingMedia(c[0], "camera", c[1], bridge.Video, nil); !errors.Is(err, bridge.ErrNotFound) {
				t.Errorf("%s/%s: %v, want not found", c[0], c[1], err)
			}
		}

		delete(l.s3, "s3.test") // S3 unreachable
		if _, _, err := media(id, bridge.Video, nil); err == nil {
			t.Error("no error from an unreachable S3")
		} else if strings.Contains(err.Error(), "Signature") || strings.Contains(err.Error(), ".mp4") {
			t.Errorf("error quotes a URL: %v", err)
		}
	})
}

// TestRecordingMediaStale lists a recording's day again once its listing is
// 20 hours old, without waiting for S3 to refuse its URL.
func TestRecordingMediaStale(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := video("CAM1", time.Now().Add(-time.Hour))
		r.URL = "" // never fetched: not found once looked up
		l := &lagging{recs: []arlo.Recording{r}}
		h, _ := home(t, fakeClient{library: l.list})
		rs, err := h.Recordings("CAM1", "camera", r.Created, r.Created)
		if err != nil || len(rs) != 1 {
			t.Fatalf("recordings: %+v, %v", rs, err)
		}
		for _, c := range []struct {
			after time.Duration
			calls int
		}{{0, 1}, {21 * time.Hour, 2}} {
			time.Sleep(c.after)
			if _, err := h.RecordingMedia("CAM1", "camera", rs[0].ID, bridge.Video, nil); !errors.Is(err, bridge.ErrNotFound) {
				t.Errorf("after %s: %v, want not found", c.after, err)
			}
			if n := l.count(); n != c.calls {
				t.Errorf("after %s: %d Library calls, want %d", c.after, n, c.calls)
			}
		}
	})
}

// TestNoticeExpires shows a recording the Library never lists, from its
// notice, for 20 hours.
func TestNoticeExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now().Add(-30 * time.Second)
		h, emit := home(t, fakeClient{library: (&lagging{}).list})
		emit(arlo.RecordingAdded{Recording: video("CAM1", start)})
		for _, c := range []struct {
			after time.Duration
			want  int
		}{{time.Minute, 1}, {20 * time.Hour, 0}} {
			time.Sleep(c.after)
			synctest.Wait()
			if rs, err := h.Recordings("CAM1", "camera", start, start); err != nil || len(rs) != c.want {
				t.Errorf("after %s: %+v, %v; want %d", c.after, rs, err, c.want)
			}
		}
	})
}
