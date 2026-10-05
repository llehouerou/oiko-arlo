package oikoarlo

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	arlo "github.com/llehouerou/go-arlo"

	"github.com/llehouerou/oiko/bridge"
)

// port records what a Bridge hands Oiko.
type port struct {
	devices      []bridge.Device
	online       []bool // each SetOnline, in order
	availability map[string]bridge.Availability
	values       map[string]any // the last reading, by address/function/capability
	replayed     int
}

func (p *port) SyncDevices(devices []bridge.Device) { p.devices = devices }
func (p *port) SetOnline(online bool)               { p.online = append(p.online, online) }
func (p *port) SetAvailability(address string, a bridge.Availability) {
	p.availability[address] = a
}
func (p *port) Report(address string, readings []bridge.Reading, at time.Time) {
	for _, r := range readings {
		p.values[address+"/"+r.Function+"/"+r.Capability] = r.Data
	}
}
func (p *port) Replayed() { p.replayed++ }

// TestHandleFeedsPort replays what go-arlo reports on connecting, as observed
// on a real account, then a motion, a custom mode and the stream going down.
func TestHandleFeedsPort(t *testing.T) {
	p := &port{availability: map[string]bridge.Availability{}, values: map[string]any{}}
	b := &Bridge{port: p}
	up, down, battery := true, false, 31

	for _, e := range []arlo.Event{
		arlo.Devices{
			{ID: "BASE", Name: "House", Model: "VMB4000", Type: "basestation"},
			{ID: "CAM1", Name: "Gate", Model: "VMC4030P", Type: "camera", BaseID: "BASE"},
			{ID: "CAM2", Name: "Veranda", Model: "VMC4030P", Type: "camera", BaseID: "BASE"},
		},
		arlo.Connection{Up: true},
		arlo.DeviceState{ID: "BASE", Connected: &up},
	} {
		b.handle(e)
	}
	if p.replayed != 0 {
		t.Fatal("replayed before the mode is known")
	}
	b.handle(arlo.ModeChanged{LocationID: "loc", LocationName: "Home", Mode: arlo.Standby})
	if p.replayed == 0 {
		t.Fatal("not replayed once the mode is known")
	}
	if got := p.values["BASE/arming/mode"]; got != "standby" {
		t.Errorf("mode = %v, want standby", got)
	}

	for _, e := range []arlo.Event{
		arlo.DeviceState{ID: "CAM1", Connected: &up, Battery: &battery},
		arlo.DeviceState{ID: "CAM2", Connected: &down},
		arlo.Motion{ID: "CAM1", Active: true},
		arlo.Motion{ID: "CAM1", Active: true}, // the base sends each packet twice
		arlo.ModeChanged{Mode: "custom"},
	} {
		b.handle(e)
	}

	var names []string
	for _, d := range p.devices {
		names = append(names, d.NativeAddress+" "+d.Name+" "+d.Model+" "+d.Vendor)
	}
	if want := []string{"BASE House VMB4000 Arlo", "CAM1 Gate VMC4030P Arlo", "CAM2 Veranda VMC4030P Arlo"}; !slices.Equal(names, want) {
		t.Errorf("devices = %q, want %q", names, want)
	}
	for key, want := range map[string]any{
		"BASE/arming/mode":         "custom", // shown, though no option
		"CAM1//battery":            31.0,
		"CAM1/occupancy/occupancy": true,
	} {
		if got := p.values[key]; got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	for addr, want := range map[string]bridge.Availability{"BASE": bridge.Online, "CAM1": bridge.Online, "CAM2": bridge.Offline} {
		if got := p.availability[addr]; got != want {
			t.Errorf("%s: %s, want %s", addr, got, want)
		}
	}

	b.handle(arlo.Connection{Up: false})
	if !slices.Equal(p.online, []bool{true, false}) {
		t.Errorf("SetOnline calls = %v, want [true false]", p.online)
	}
}

// TestDescribe checks the Capabilities a base station and a camera get; the
// arming mode offers only the modes that can be commanded.
func TestDescribe(t *testing.T) {
	base := describe(arlo.Device{ID: "BASE", Type: "basestation"})
	if len(base.Functions) != 1 || base.Functions[0].Key != "arming" || len(base.Functions[0].Capabilities) != 1 {
		t.Fatalf("base: %+v", base)
	}
	mode := base.Functions[0].Capabilities[0]
	if mode.Key != "mode" || mode.Type != bridge.Enum || !mode.Access.Settable {
		t.Errorf("mode: %+v", mode)
	}
	if want := []string{"standby", "armHome", "armAway"}; !slices.Equal(mode.Options, want) {
		t.Errorf("mode options = %q, want %q (never custom)", mode.Options, want)
	}

	cam := describe(arlo.Device{ID: "CAM1", Type: "camera"})
	if len(cam.Functions) != 1 || cam.Functions[0].Key != "occupancy" || cam.Functions[0].Capabilities[0].Type != bridge.Binary {
		t.Errorf("camera functions: %+v", cam.Functions)
	}
	if len(cam.Capabilities) != 1 || cam.Capabilities[0].Key != "battery" || cam.Capabilities[0].Category != bridge.Diagnostic {
		t.Errorf("camera capabilities: %+v", cam.Capabilities)
	}
}

// TestManifest checks the catalogue's manifest: the types registered, and an
// example section made of Config's keys only.
func TestManifest(t *testing.T) {
	data, err := os.ReadFile("oiko-bridge.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Types map[string]struct {
			Description string
			Config      json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, r := range bridge.Types() {
		types = append(types, r.Type)
	}
	if len(m.Types) != len(types) {
		t.Errorf("manifest types: %d, registered: %q", len(m.Types), types)
	}
	for _, typ := range types {
		section, ok := m.Types[typ]
		if !ok {
			t.Errorf("type %s missing from the manifest", typ)
			continue
		}
		d := json.NewDecoder(bytes.NewReader(section.Config))
		d.DisallowUnknownFields()
		var c Config
		if err := d.Decode(&c); err != nil {
			t.Errorf("type %s: example section: %v", typ, err)
		}
	}
}
