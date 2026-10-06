package oikoarlo

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	arlo "github.com/llehouerou/go-arlo"
	"github.com/llehouerou/oiko/bridge/bridgetest"
)

// fakeClient answers the Bridge's calls from memory, and hands Run's handler
// the events run's emit sends.
type fakeClient struct {
	images  map[string]arlo.LastImages
	stream  string
	library func(from, to time.Time) []arlo.Recording
	events  chan arlo.Event
	handled chan struct{}
}

func (f fakeClient) Library(_ context.Context, from, to time.Time) ([]arlo.Recording, error) {
	return f.library(from, to), nil
}

func (f fakeClient) Run(ctx context.Context, handle func(arlo.Event)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e := <-f.events:
			handle(e)
			f.handled <- struct{}{}
		}
	}
}

func (fakeClient) SetMode(context.Context, arlo.Mode) error { return nil }

func (f fakeClient) LastImages(_ context.Context, id string) (arlo.LastImages, error) {
	li, ok := f.images[id]
	if !ok {
		return li, errors.New("no camera " + id)
	}
	return li, nil
}

func (f fakeClient) Stream(_ context.Context, id string) (string, error) {
	if _, ok := f.images[id]; !ok {
		return "", errors.New("no camera " + id)
	}
	return f.stream, nil
}

const secret = "X-Amz-Signature=s3cr3t"

// cameras is a home with the Bridge online, its base BASE and camera CAM1,
// whose pictures are images.
func cameras(t *testing.T, images arlo.LastImages) *bridgetest.Home {
	h, _ := home(t, fakeClient{images: map[string]arlo.LastImages{"CAM1": images}})
	return h
}

// home is a home with the Bridge on c online, its base BASE and cameras
// CAM1 and CAM2.
func home(t *testing.T, c fakeClient) (*bridgetest.Home, func(...arlo.Event)) {
	h, emit := run(t, c)
	emit(arlo.Devices{
		{ID: "BASE", Name: "House", Type: "basestation"},
		{ID: "CAM1", Name: "Gate", Type: "camera", BaseID: "BASE"},
		{ID: "CAM2", Name: "Veranda", Type: "camera", BaseID: "BASE"},
	}, arlo.Connection{Up: true})
	return h, emit
}

// run runs a Bridge on c as Oiko does, until the test ends. emit hands it
// events as go-arlo does, each one handled once emit returns.
func run(t *testing.T, c fakeClient) (h *bridgetest.Home, emit func(...arlo.Event)) {
	c.events, c.handled = make(chan arlo.Event), make(chan struct{})
	b := newBridge(c, slog.New(slog.DiscardHandler))
	h = bridgetest.New(b)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		b.Run(ctx, h.Port())
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	return h, func(es ...arlo.Event) {
		for _, e := range es {
			c.events <- e
			<-c.handled
		}
	}
}

// TestPicture serves an older thumbnail and a newer snapshot, and checks the
// more recent wins, the other stands in for a missing one, and no error
// quotes a presigned URL.
func TestPicture(t *testing.T) {
	older := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/image.jpg":
			w.Header().Set("Last-Modified", older.Format(http.TimeFormat))
			w.Write([]byte("image"))
		case "/snapshot.jpg":
			w.Header().Set("Last-Modified", newer.Format(http.TimeFormat))
			w.Write([]byte("snapshot"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	image, snapshot, gone := srv.URL+"/image.jpg?"+secret, srv.URL+"/snapshot.jpg?"+secret, srv.URL+"/gone.jpg?"+secret

	for _, c := range []struct {
		name   string
		images arlo.LastImages
		want   string
		taken  time.Time
	}{
		{"snapshot newer", arlo.LastImages{Image: image, Snapshot: snapshot}, "snapshot", newer},
		{"image newer", arlo.LastImages{Image: snapshot, Snapshot: image}, "snapshot", newer},
		{"no snapshot", arlo.LastImages{Image: image}, "image", older},
		{"snapshot gone", arlo.LastImages{Image: image, Snapshot: gone}, "image", older},
		{"image gone", arlo.LastImages{Image: gone, Snapshot: snapshot}, "snapshot", newer},
	} {
		pic, err := cameras(t, c.images).Picture("CAM1", "camera")
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if string(pic.Data) != c.want || pic.ContentType != "image/jpeg" || !pic.Taken.Equal(c.taken) {
			t.Errorf("%s: %q %s %s, want %q taken %s", c.name, pic.Data, pic.ContentType, pic.Taken, c.want, c.taken)
		}
	}

	srv.Close() // a connection error too
	for _, images := range []arlo.LastImages{{Image: gone, Snapshot: gone}, {Image: image, Snapshot: snapshot}} {
		_, err := cameras(t, images).Picture("CAM1", "camera")
		if err == nil {
			t.Errorf("%+v: no error", images)
		} else if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), ".jpg") {
			t.Errorf("error quotes a URL: %v", err)
		}
	}
}

// TestStream checks the scheme Oiko gets.
func TestStream(t *testing.T) {
	b := newBridge(fakeClient{images: map[string]arlo.LastImages{"CAM1": {}}, stream: "rtsps://192.0.2.1:443/stream?" + secret}, slog.New(slog.DiscardHandler))
	u, err := b.Stream(context.Background(), "CAM1", "camera")
	if want := "rtspx://192.0.2.1:443/stream?" + secret; err != nil || u != want {
		t.Errorf("stream = %q, %v; want %q", u, err, want)
	}
}

// TestNoCameraRefused: Oiko refuses what is no camera Function before the
// Bridge, which never checks it.
func TestNoCameraRefused(t *testing.T) {
	h := cameras(t, arlo.LastImages{})
	if _, err := h.Picture("BASE", "arming"); !errors.Is(err, bridgetest.ErrRefused) {
		t.Errorf("picture of the base's arming: %v, want refused", err)
	}
	if _, err := h.Recordings("CAM1", "occupancy", time.Now(), time.Now()); !errors.Is(err, bridgetest.ErrRefused) {
		t.Errorf("recordings of a camera's occupancy: %v, want refused", err)
	}
}
