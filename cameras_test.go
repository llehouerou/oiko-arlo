package oikoarlo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	arlo "github.com/llehouerou/go-arlo"
	"github.com/llehouerou/oiko/bridge/bridgetest"
)

// fakeClient answers the cameras' calls from memory.
type fakeClient struct {
	images  map[string]arlo.LastImages
	stream  string
	library func(from, to time.Time) []arlo.Recording
}

func (f fakeClient) Library(_ context.Context, from, to time.Time) ([]arlo.Recording, error) {
	return f.library(from, to), nil
}

func (fakeClient) Run(context.Context, func(arlo.Event)) error { return nil }
func (fakeClient) SetMode(context.Context, arlo.Mode) error    { return nil }

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
	return home(fakeClient{images: map[string]arlo.LastImages{"CAM1": images}})
}

// home is a home with the Bridge on c online, its base BASE and cameras
// CAM1 and CAM2.
func home(c fakeClient) *bridgetest.Home {
	b := &Bridge{client: c}
	h := bridgetest.New(b)
	b.port = h.Port()
	b.handle(arlo.Devices{
		{ID: "BASE", Name: "House", Type: "basestation"},
		{ID: "CAM1", Name: "Gate", Type: "camera", BaseID: "BASE"},
		{ID: "CAM2", Name: "Veranda", Type: "camera", BaseID: "BASE"},
	})
	b.handle(arlo.Connection{Up: true})
	return h
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

// TestStream checks the scheme Oiko gets, and the refusals of what is no
// camera.
func TestStream(t *testing.T) {
	b := &Bridge{client: fakeClient{images: map[string]arlo.LastImages{"CAM1": {}}, stream: "rtsps://192.0.2.1:443/stream?" + secret}}
	u, err := b.Stream(context.Background(), "CAM1", "camera")
	if want := "rtspx://192.0.2.1:443/stream?" + secret; err != nil || u != want {
		t.Errorf("stream = %q, %v; want %q", u, err, want)
	}
	for _, c := range [][2]string{{"BASE", "camera"}, {"CAM9", "camera"}, {"CAM1", "occupancy"}} {
		if _, err := b.Stream(context.Background(), c[0], c[1]); err == nil {
			t.Errorf("%s/%s: no error", c[0], c[1])
		}
		if _, err := b.Picture(context.Background(), c[0], c[1]); err == nil {
			t.Errorf("%s/%s: picture, no error", c[0], c[1])
		}
	}
	if _, err := cameras(t, arlo.LastImages{}).Picture("BASE", "arming"); !errors.Is(err, bridgetest.ErrRefused) {
		t.Errorf("picture of the base's arming: %v, want refused", err)
	}
}
