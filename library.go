package oikoarlo

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	arlo "github.com/llehouerou/go-arlo"

	"github.com/llehouerou/oiko/bridge"
)

const (
	// keepListed is how long a listing's presigned URLs, valid 24 hours, are
	// used before the Library is listed again.
	keepListed = 20 * time.Hour
	// keepAnnounced is how long after its start a recording is remembered as
	// announced: long past a repeat notice or a backfill.
	keepAnnounced = time.Hour
	day           = 24 * time.Hour
)

// recordingKey finds a recording: its camera and its start in Unix ms.
type recordingKey struct {
	camera string
	start  int64
}

func keyOf(r arlo.Recording) recordingKey { return recordingKey{r.CameraID, r.Created.UnixMilli()} }

type keptRecording struct {
	arlo.Recording
	at      time.Time // when listed or noticed
	noticed bool      // known from its notice only, the Library not having listed it
}

// library is Arlo's Library as the Bridge sees it: the videos a listing or a
// notice of the last 20 hours holds, with their presigned URLs, and the
// recordings announced. It lists every camera's recordings of whole days in
// the location's time zone, so the days asked for are widened by one on each
// side.
type library struct {
	list func(ctx context.Context, from, to time.Time) ([]arlo.Recording, error)
	s3   *http.Client

	mu        sync.Mutex
	kept      map[recordingKey]keptRecording
	announced map[recordingKey]time.Time // by the recording's start
}

func newLibrary(list func(ctx context.Context, from, to time.Time) ([]arlo.Recording, error), s3 *http.Client) *library {
	return &library{list: list, s3: s3, kept: map[recordingKey]keptRecording{}, announced: map[recordingKey]time.Time{}}
}

// videos returns every camera's videos started within [from, to]: a fresh
// listing's, and those noticed it does not hold yet.
func (l *library) videos(ctx context.Context, from, to time.Time) ([]arlo.Recording, error) {
	recs, err := l.relist(ctx, from, to)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	for _, k := range l.kept { // relist dropped the stale ones
		if k.noticed {
			recs = append(recs, k.Recording)
		}
	}
	l.mu.Unlock()
	return slices.DeleteFunc(recs, func(r arlo.Recording) bool { return r.Created.Before(from) || r.Created.After(to) }), nil
}

// listed returns camera's video started at start, once the Library lists it:
// never from its notice, which lacks what triggered it.
func (l *library) listed(ctx context.Context, camera string, start time.Time) (arlo.Recording, bool) {
	recs, _ := l.relist(ctx, start, start) // a failure is a later try
	i := slices.IndexFunc(recs, func(r arlo.Recording) bool { return r.CameraID == camera && r.Created.Equal(start) })
	if i < 0 {
		return arlo.Recording{}, false
	}
	return recs[i], true
}

// media GETs part of camera's video started at start, with header. Its URL
// comes from a listing or a notice of the last 20 hours, else from listing
// its day once more, as when S3 refuses an expired URL. bridge.ErrNotFound
// when the Library has no such video; no error holds a URL.
func (l *library) media(ctx context.Context, camera string, start time.Time, part bridge.RecordingPart, header http.Header) (*http.Response, error) {
	key, relisted := recordingKey{camera, start.UnixMilli()}, false
	for {
		l.mu.Lock()
		k, ok := l.kept[key]
		l.mu.Unlock()
		ok = ok && time.Since(k.at) <= keepListed
		if !ok && !relisted {
			if _, err := l.relist(ctx, start, start); err != nil {
				return nil, err
			}
			relisted = true
			continue
		}
		u := k.URL
		if part == bridge.Thumbnail {
			u = k.ThumbnailURL
		}
		if !ok || u == "" {
			return nil, bridge.ErrNotFound
		}
		resp, err := get(ctx, l.s3, u, header)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusForbidden && !relisted { // expired
			resp.Body.Close()
			l.mu.Lock()
			delete(l.kept, key)
			l.mu.Unlock()
			continue
		}
		return resp, nil
	}
}

// notice records a recording about to be announced, keeping its URLs unless
// a listing holds them already; false if it was announced already.
func (l *library) notice(r arlo.Recording) bool {
	key := keyOf(r)
	l.mu.Lock()
	defer l.mu.Unlock()
	maps.DeleteFunc(l.announced, func(_ recordingKey, start time.Time) bool { return time.Since(start) > keepAnnounced })
	if _, done := l.announced[key]; done {
		return false
	}
	l.announced[key] = r.Created
	if _, ok := l.kept[key]; !ok {
		l.kept[key] = keptRecording{Recording: r, at: time.Now(), noticed: true}
	}
	return true
}

// relist lists the videos of the days from to to, widened by one on each
// side, and keeps their URLs.
func (l *library) relist(ctx context.Context, from, to time.Time) ([]arlo.Recording, error) {
	// go-arlo waits for Run's connection, which may sit in a long backoff.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	recs, err := l.list(ctx, from.Add(-day), to.Add(day))
	if err != nil {
		return nil, err
	}
	recs = slices.DeleteFunc(recs, func(r arlo.Recording) bool { return r.ContentType != "video/mp4" })
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	maps.DeleteFunc(l.kept, func(_ recordingKey, k keptRecording) bool { return now.Sub(k.at) > keepListed })
	for _, r := range recs {
		l.kept[keyOf(r)] = keptRecording{Recording: r, at: now}
	}
	return recs, nil
}
