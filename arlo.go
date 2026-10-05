// Package oikoarlo is the arlo type of Bridge of Oiko: it connects Oiko to
// Arlo's cloud through go-arlo (Oiko's ADR 0010). Each base station carries
// its location's mode, each camera its motion, battery and connection.
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
	"log/slog"
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
	client *arlo.Client
	log    *slog.Logger
	// Only Run's goroutine touches these.
	port  bridge.Port
	bases []string // Native Addresses of the base stations: they carry the mode
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
	return &Bridge{client: arlo.New(arlo.Config{
		Email:       c.Email,
		Password:    password,
		SessionPath: filepath.Join(env.DataDir, "session.json"),
		Code:        arlo.IMAPCode(c.IMAPServer, c.IMAPUser, imapPassword),
		DumpDir:     c.DumpDir,
		Log:         env.Log,
	}), log: env.Log}, nil
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
	b.port = p
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

// handle translates one go-arlo event into Oiko's terms.
func (b *Bridge) handle(e arlo.Event) {
	now := time.Now()
	switch e := e.(type) {
	case arlo.Connection:
		b.port.SetOnline(e.Up)
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
		}}}}
		dev.Capabilities = []bridge.Capability{{
			Key: "battery", Label: "Battery", Type: bridge.Numeric, Unit: "%",
			Min: &percent[0], Max: &percent[1], Access: observable, Category: bridge.Diagnostic,
		}}
	}
	return dev
}
