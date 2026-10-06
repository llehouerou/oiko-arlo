package oikoarlo

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	arlo "github.com/llehouerou/go-arlo"

	"github.com/llehouerou/oiko/bridge"
	"github.com/llehouerou/oiko/bridge/bridgetest"
)

// library is a fake of Arlo's library: its entries' URLs point at an S3
// stand-in that refuses a signature other than the latest listing's.
type library struct {
	srv     *httptest.Server
	base    time.Time // the start of CAM1's first recording in range
	calls   [][2]time.Time
	mu      sync.Mutex
	sig     string // the latest listing's signature
	nextSig string
}

func (l *library) signature() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sig
}

func newLibrary(t *testing.T) *library {
	l := &library{base: time.Now().Truncate(time.Hour).Add(-48 * time.Hour), nextSig: "s1"}
	l.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("X-Amz-Signature") != l.signature() {
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		http.ServeContent(w, r, r.URL.Path, l.base, bytes.NewReader([]byte("media of "+r.URL.Path)))
	}))
	t.Cleanup(l.srv.Close)
	return l
}

// list answers Library: CAM1's three videos and a snapshot in range, one
// video before it, and CAM2's video.
func (l *library) list(from, to time.Time) []arlo.Recording {
	l.calls = append(l.calls, [2]time.Time{from, to})
	l.mu.Lock()
	l.sig = l.nextSig
	l.mu.Unlock()
	rec := func(cam string, created time.Time, ct, reason, object string) arlo.Recording {
		u := l.srv.URL + "/" + cam + "/" + recordingID(arlo.Recording{Created: created})
		return arlo.Recording{CameraID: cam, Created: created, Duration: 20 * time.Second, ContentType: ct, Reason: reason, Object: object,
			URL: u + ".mp4?X-Amz-Signature=" + l.sig, ThumbnailURL: u + ".jpg?X-Amz-Signature=" + l.sig}
	}
	return []arlo.Recording{
		rec("CAM1", l.base.Add(-5*time.Hour), "video/mp4", "motionRecord", ""),
		rec("CAM1", l.base, "video/mp4", "motionRecord", "Person"),
		rec("CAM1", l.base.Add(30*time.Minute), "image/jpg", "", ""),
		rec("CAM1", l.base.Add(time.Hour), "video/mp4", "motionRecord", ""),
		rec("CAM2", l.base.Add(time.Hour), "video/mp4", "motionRecord", ""),
		rec("CAM1", l.base.Add(2*time.Hour), "video/mp4", "audioRecord", ""),
	}
}

func (l *library) home() *bridgetest.Home { return home(fakeClient{library: l.list}) }

// TestRecordings lists CAM1's videos in range, the newest first, from the
// days around it, and clamps a range from 1970 to Arlo's retention.
func TestRecordings(t *testing.T) {
	l := newLibrary(t)
	h := l.home()
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
	if want := []string{"2h0m0s sound 20s", "1h0m0s motion 20s", "0s person 20s"}; !slices.Equal(got, want) {
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
}

func TestTrigger(t *testing.T) {
	for _, c := range []struct{ reason, object, want string }{
		{"motionRecord", "Vehicle", "vehicle"},
		{"motionRecord", "", "motion"},
		{"audioRecord", "", "sound"},
		{"userRecord", "", ""},
	} {
		if got := trigger(arlo.Recording{Reason: c.reason, Object: c.object}); got != c.want {
			t.Errorf("trigger(%q, %q) = %q, want %q", c.reason, c.object, got, c.want)
		}
	}
}

// TestRecordingMedia fetches a listed video's range, its thumbnail, an
// unlisted one, a refused one, and checks no error quotes a URL.
func TestRecordingMedia(t *testing.T) {
	l := newLibrary(t)
	h := l.home()
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
	resp, err := l.home().RecordingMedia("CAM1", "camera", other, bridge.Video, nil)
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

	l.srv.Close()
	if _, _, err := media(id, bridge.Video, nil); err == nil {
		t.Error("no error from a closed S3")
	} else if strings.Contains(err.Error(), "Signature") || strings.Contains(err.Error(), ".mp4") {
		t.Errorf("error quotes a URL: %v", err)
	}
}
