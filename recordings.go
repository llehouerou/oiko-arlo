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

const (
	// retention is how far back Arlo keeps recordings, at most.
	retention = 31 * 24 * time.Hour
	// keepListed is how long a listing's presigned URLs, valid 24 hours, are
	// used before the library is listed again.
	keepListed = 20 * time.Hour
	day        = 24 * time.Hour
)

// recordingKey finds a recording: its camera and ID.
type recordingKey struct{ camera, id string }

type listedRecording struct {
	arlo.Recording
	at time.Time // when listed
}

// Recordings lists a camera's videos, from Arlo's library, which lists every
// camera's recordings of whole days in the location's time zone: the days
// asked for are widened by one on each side. The ID of a recording is its
// start in Unix milliseconds.
func (b *Bridge) Recordings(ctx context.Context, address, function string, from, to time.Time) ([]bridge.Recording, error) {
	if function != camera {
		return nil, fmt.Errorf("arlo: no camera Function %q: %w", function, bridge.ErrNotFound)
	}
	if oldest := time.Now().Add(-retention); from.Before(oldest) {
		from = oldest // Oiko's "All" asks from 1970
	}
	if to.Before(from) {
		return nil, nil
	}
	recs, err := b.list(ctx, from, to)
	if err != nil {
		return nil, err
	}
	var out []bridge.Recording
	for _, r := range recs {
		if r.CameraID == address && !r.Created.Before(from) && !r.Created.After(to) {
			out = append(out, bridge.Recording{ID: recordingID(r), Start: r.Created, Duration: r.Duration, Trigger: trigger(r)})
		}
	}
	slices.SortFunc(out, func(a, b bridge.Recording) int { return b.Start.Compare(a.Start) })
	return out, nil
}

// RecordingMedia GETs a recording's video or thumbnail with header. Its URL
// comes from a listing of the last 20 hours, else from listing its day once
// more, as when S3 refuses an expired URL.
func (b *Bridge) RecordingMedia(ctx context.Context, address, function, id string, part bridge.RecordingPart, header http.Header) (*http.Response, error) {
	ms, err := strconv.ParseInt(id, 10, 64)
	if function != camera || err != nil {
		return nil, fmt.Errorf("arlo: recording %s of %s: %w", id, address, bridge.ErrNotFound)
	}
	key, relisted := recordingKey{address, id}, false
	for {
		r, ok := b.lookUp(key)
		if !ok && !relisted {
			start := time.UnixMilli(ms)
			if _, err := b.list(ctx, start, start); err != nil {
				return nil, err
			}
			relisted = true
			continue
		}
		u := r.URL
		if part == bridge.Thumbnail {
			u = r.ThumbnailURL
		}
		if !ok || u == "" {
			return nil, fmt.Errorf("arlo: %s of recording %s of %s: %w", part, id, address, bridge.ErrNotFound)
		}
		resp, err := get(ctx, u, header)
		if err != nil {
			return nil, fmt.Errorf("arlo: %s of recording %s of %s: %w", part, id, address, err)
		}
		if resp.StatusCode == http.StatusForbidden && !relisted { // expired
			resp.Body.Close()
			b.mu.Lock()
			delete(b.listed, key)
			b.mu.Unlock()
			continue
		}
		return resp, nil
	}
}

// list returns the videos of the days from to to, widened by one on each
// side, and keeps their URLs for RecordingMedia.
func (b *Bridge) list(ctx context.Context, from, to time.Time) ([]arlo.Recording, error) {
	// go-arlo waits for Run's connection, which may sit in a long backoff.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	recs, err := b.client.Library(ctx, from.Add(-day), to.Add(day))
	if err != nil {
		return nil, fmt.Errorf("arlo: %w", err)
	}
	recs = slices.DeleteFunc(recs, func(r arlo.Recording) bool { return r.ContentType != "video/mp4" })
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.listed == nil {
		b.listed = map[recordingKey]listedRecording{}
	}
	for k, r := range b.listed {
		if now.Sub(r.at) > keepListed {
			delete(b.listed, k)
		}
	}
	for _, r := range recs {
		b.listed[recordingKey{r.CameraID, recordingID(r)}] = listedRecording{r, now}
	}
	return recs, nil
}

// lookUp finds a recording a listing of the last 20 hours holds.
func (b *Bridge) lookUp(k recordingKey) (arlo.Recording, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.listed[k]
	if !ok || time.Since(r.at) > keepListed {
		return arlo.Recording{}, false
	}
	return r.Recording, true
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
