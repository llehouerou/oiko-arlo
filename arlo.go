// Package oikoarlo is the arlo type of Bridge of Oiko: it connects Oiko to
// Arlo's cloud through go-arlo (Oiko's ADR 0010). Each base station carries
// its location's mode, each camera its motion, battery and connection, and
// its Picture and Live view (bridge.Cameras) and its Recordings
// (bridge.Recordings).
//
// An Oiko built with this module imports it for its side effect, which
// registers the type:
//
//	import _ "github.com/llehouerou/oiko-arlo"
package oikoarlo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	arlo "github.com/llehouerou/go-arlo"

	"github.com/llehouerou/oiko/bridge"
)

func init() { bridge.Register(bridge.Module{Type: "arlo", New: open}) }

// Config is an arlo Bridge's section of Oiko's configuration. Passwords are
// read from files, so that they stay out of it.
type Config struct {
	Email            string `json:"email"`
	PasswordFile     string `json:"passwordFile"`
	IMAPServer       string `json:"imapServer"` // host:port, where Arlo's two-factor codes arrive
	IMAPUser         string `json:"imapUser"`
	IMAPPasswordFile string `json:"imapPasswordFile"`
	// DumpDir, a debugging aid, receives one JSON file per HTTP response and
	// MQTT message, secrets redacted, with no rotation. Empty disables it.
	DumpDir string `json:"dumpDir"`
}

// Bridge is Oiko's Arlo account.
type Bridge struct {
	client client
	lib    *library
	log    *slog.Logger
	// Only Run's goroutine touches these.
	ctx   context.Context // Run's, which ends the announcements
	port  bridge.Port
	bases []string // Native Addresses of the base stations: they carry the mode
	wasUp bool     // connected before: a reconnection backfills announcements
}

// client is the part of go-arlo's Client the Bridge uses.
type client interface {
	Run(ctx context.Context, handle func(arlo.Event)) error
	SetMode(ctx context.Context, mode arlo.Mode) error
	LastImages(ctx context.Context, cameraID string) (arlo.LastImages, error)
	Stream(ctx context.Context, cameraID string) (string, error)
}

// open reads the passwords and returns a Bridge keeping its session in the
// data directory. It must never be copied (see go-arlo's README).
func open(env bridge.Env) (bridge.Bridge, error) {
	var c Config
	if err := env.Decode(&c); err != nil {
		return nil, fmt.Errorf("arlo: %w", err)
	}
	if c.Email == "" || c.IMAPServer == "" || c.IMAPUser == "" {
		return nil, errors.New("arlo: email, imapServer and imapUser are required")
	}
	password, err := readSecret(c.PasswordFile)
	if err != nil {
		return nil, err
	}
	imapPassword, err := readSecret(c.IMAPPasswordFile)
	if err != nil {
		return nil, err
	}
	ac := arlo.New(arlo.Config{
		Email:       c.Email,
		Password:    password,
		SessionPath: filepath.Join(env.DataDir, "session.json"),
		Code:        arlo.IMAPCode(c.IMAPServer, c.IMAPUser, imapPassword),
		DumpDir:     c.DumpDir,
		Log:         env.Log,
	})
	return &Bridge{client: ac, lib: newLibrary(ac.Library), log: env.Log}, nil
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("arlo: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// Run follows Arlo's event stream and feeds Oiko through p until ctx is
// cancelled. go-arlo reconnects by itself, sparing Arlo's auth rate limit:
// never retry around it.
func (b *Bridge) Run(ctx context.Context, p bridge.Port) {
	b.ctx, b.port = ctx, p
	if err := b.client.Run(ctx, b.handle); ctx.Err() == nil {
		b.log.Error("stopped", "err", err)
	}
}

// Send sets the location's mode, until ctx ends with the Command's timeout;
// the ModeChanged go-arlo then reports confirms the Command. Oiko only lets
// through the one settable Capability.
func (b *Bridge) Send(ctx context.Context, address, function string, values map[string]any, transition time.Duration) error {
	mode, _ := values["mode"].(string)
	return b.client.SetMode(ctx, arlo.Mode(mode))
}

var _ bridge.Cameras = (*Bridge)(nil)

// camera is the key and kind of each camera's Function.
const camera = "camera"

// maxPicture bounds the body of a picture's GET.
const maxPicture = 8 << 20

// Picture returns the more recent of a camera's two latest pictures, its
// recording thumbnail and its full-frame snapshot, as their Last-Modified
// dates them. go-arlo keeps their URLs in memory: the camera never wakes.
// The URLs are presigned, so they never appear in an error, which Oiko logs.
func (b *Bridge) Picture(ctx context.Context, address, function string) (bridge.Picture, error) {
	if function != camera {
		return bridge.Picture{}, fmt.Errorf("arlo: no camera Function %q", function)
	}
	// go-arlo waits for Run's connection, which may sit in a long backoff.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	li, err := b.client.LastImages(ctx, address)
	if err != nil {
		return bridge.Picture{}, fmt.Errorf("arlo: %w", err)
	}
	image, imageErr := fetch(ctx, "image", li.Image)
	snapshot, snapshotErr := fetch(ctx, "snapshot", li.Snapshot)
	switch {
	case imageErr != nil && snapshotErr != nil:
		return bridge.Picture{}, fmt.Errorf("arlo: %s: %w", address, errors.Join(imageErr, snapshotErr))
	case imageErr != nil || snapshot.Taken.After(image.Taken):
		return snapshot, nil
	}
	return image, nil
}

// fetch GETs the presigned JPEG at u; its errors leave u out.
func fetch(ctx context.Context, name, u string) (bridge.Picture, error) {
	if u == "" {
		return bridge.Picture{}, fmt.Errorf("%s: none", name)
	}
	resp, err := get(ctx, u, nil)
	if err != nil {
		return bridge.Picture{}, fmt.Errorf("%s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return bridge.Picture{}, fmt.Errorf("%s: %s", name, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPicture+1))
	if err != nil {
		return bridge.Picture{}, fmt.Errorf("%s: %w", name, err)
	}
	if len(data) > maxPicture {
		return bridge.Picture{}, fmt.Errorf("%s: over %d bytes", name, maxPicture)
	}
	taken, _ := http.ParseTime(resp.Header.Get("Last-Modified"))
	return bridge.Picture{Data: data, ContentType: "image/jpeg", Taken: taken}, nil
}

// presigned GETs Arlo's presigned URLs. It bounds the wait for the headers
// but not the body, which may be a video read for as long as it plays, and
// leaves the body as S3 sends it.
var presigned = func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 30 * time.Second
	t.DisableCompression = true
	return &http.Client{Transport: t}
}()

// get GETs the presigned URL u with header; its errors leave u out.
func get(ctx context.Context, u string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.New("invalid URL")
	}
	if header != nil {
		req.Header = header.Clone()
	}
	resp, err := presigned.Do(req)
	if ue := (*url.Error)(nil); errors.As(err, &ue) {
		err = ue.Err // url.Error quotes the URL
	}
	return resp, err
}

// Stream starts a camera's live stream, or joins the one running. Arlo's
// RTSPS endpoint is an IP address its certificate does not name: rtspx://
// tells Oiko not to verify it. The URL is a credential: never log it.
func (b *Bridge) Stream(ctx context.Context, address, function string) (string, error) {
	if function != camera {
		return "", fmt.Errorf("arlo: no camera Function %q", function)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second) // as for Picture
	defer cancel()
	u, err := b.client.Stream(ctx, address)
	if err != nil {
		return "", fmt.Errorf("arlo: %w", err)
	}
	rest, ok := strings.CutPrefix(u, "rtsps://")
	if !ok {
		return "", errors.New("arlo: stream URL not rtsps")
	}
	return "rtspx://" + rest, nil
}

// handle translates one go-arlo event into Oiko's terms.
func (b *Bridge) handle(e arlo.Event) {
	now := time.Now()
	switch e := e.(type) {
	case arlo.Connection:
		b.port.SetOnline(e.Up)
		if e.Up && b.wasUp {
			go b.backfill(b.ctx)
		}
		b.wasUp = b.wasUp || e.Up
	case arlo.RecordingAdded:
		if e.ContentType == "video/mp4" && b.lib.notice(e.Recording) {
			go b.announce(b.ctx, e.Recording)
		}
	case arlo.Devices:
		b.bases = b.bases[:0]
		devices := make([]bridge.Device, 0, len(e))
		for _, d := range e {
			devices = append(devices, describe(d))
			if d.Type == "basestation" {
				b.bases = append(b.bases, d.ID)
			}
		}
		b.port.SyncDevices(devices)
	case arlo.DeviceState:
		if e.Connected != nil {
			b.port.SetAvailability(e.ID, map[bool]bridge.Availability{true: bridge.Online, false: bridge.Offline}[*e.Connected])
		}
		if e.Battery != nil {
			b.port.Report(e.ID, []bridge.Reading{{Capability: "battery", Data: float64(*e.Battery)}}, now)
		}
	case arlo.Motion:
		b.port.Report(e.ID, []bridge.Reading{{Function: "occupancy", Capability: "occupancy", Data: e.Active}}, now)
	case arlo.ModeChanged:
		// ponytail: every base of the account's one location carries its
		// mode; a location argument if go-arlo ever follows several.
		for _, id := range b.bases {
			b.port.Report(id, []bridge.Reading{{Function: "arming", Capability: "mode", Data: string(e.Mode)}}, now)
		}
		// Read right after the base's ping and the request for its cameras'
		// state, which follows within a second or two as initial Values.
		// Later calls change nothing.
		b.port.Replayed()
	}
}

var (
	observable = bridge.Access{Observable: true}
	percent    = [2]float64{0, 100}
)

// describe is the Oiko view of an Arlo base station or camera. A custom mode
// the owner activates is reported as "custom", outside the options: shown,
// never commanded.
func describe(d arlo.Device) bridge.Device {
	dev := bridge.Device{NativeAddress: d.ID, Name: d.Name, Model: d.Model, Vendor: "Arlo"}
	switch d.Type {
	case "basestation":
		dev.Functions = []bridge.Function{{Key: "arming", Kind: "arming", Capabilities: []bridge.Capability{{
			Key: "mode", Label: "Mode", Type: bridge.Enum,
			Options:  []string{string(arlo.Standby), string(arlo.ArmHome), string(arlo.ArmAway)},
			Access:   bridge.Access{Observable: true, Settable: true},
			Category: bridge.Primary,
		}}}}
	case "camera":
		dev.Functions = []bridge.Function{{Key: "occupancy", Kind: "occupancy", Capabilities: []bridge.Capability{{
			Key: "occupancy", Label: "Occupancy", Type: bridge.Binary, Access: observable, Category: bridge.Primary,
		}}}, {Key: camera, Kind: camera, Capabilities: []bridge.Capability{{
			Key: bridge.RecordingEvent, Label: "Recording", Type: bridge.Enum, Options: triggers,
			Stateless: true, Access: observable, Category: bridge.Primary,
		}}}}
		dev.Capabilities = []bridge.Capability{{
			Key: "battery", Label: "Battery", Type: bridge.Numeric, Unit: "%",
			Min: &percent[0], Max: &percent[1], Access: observable, Category: bridge.Diagnostic,
		}}
	}
	return dev
}
