package oikoarlo

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"

	arlo "github.com/llehouerou/go-arlo"

	"github.com/llehouerou/oiko/bridge"
	"github.com/llehouerou/oiko/bridge/bridgetest"
)

// TestHandleFeedsPort replays what go-arlo reports on connecting, as observed
// on a real account, then a motion, a custom mode and the stream going down.
func TestHandleFeedsPort(t *testing.T) {
	b := &Bridge{}
	h := bridgetest.New(b)
	b.port = h.Port()
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
	if h.Replayed() {
		t.Fatal("replayed before the mode is known")
	}
	b.handle(arlo.ModeChanged{LocationID: "loc", LocationName: "Home", Mode: arlo.Standby})
	if !h.Replayed() {
		t.Fatal("not replayed once the mode is known")
	}
	if got, _ := h.Value("BASE", "arming", "mode"); got.Data != "standby" {
		t.Errorf("mode = %v, want standby", got.Data)
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
	for _, d := range h.Devices() {
		names = append(names, d.NativeAddress+" "+d.Name+" "+d.Model+" "+d.Vendor)
	}
	if want := []string{"BASE House VMB4000 Arlo", "CAM1 Gate VMC4030P Arlo", "CAM2 Veranda VMC4030P Arlo"}; !slices.Equal(names, want) {
		t.Errorf("devices = %q, want %q", names, want)
	}
	for _, c := range []struct {
		addr, fn, capability string
		want                 any
	}{
		{"BASE", "arming", "mode", "custom"}, // shown, though no option
		{"CAM1", "", "battery", 31.0},
		{"CAM1", "occupancy", "occupancy", true},
	} {
		if got, _ := h.Value(c.addr, c.fn, c.capability); got.Data != c.want {
			t.Errorf("%s/%s/%s = %v, want %v", c.addr, c.fn, c.capability, got.Data, c.want)
		}
	}
	for addr, want := range map[string]bridge.Availability{"BASE": bridge.Online, "CAM1": bridge.Online, "CAM2": bridge.Offline} {
		if got := h.Availability(addr); got != want {
			t.Errorf("%s: %s, want %s", addr, got, want)
		}
	}

	// Refused by Oiko before reaching Send, which has no client here.
	if err := h.Command("BASE", "arming", map[string]any{"mode": "custom"}); !errors.Is(err, bridgetest.ErrRefused) {
		t.Errorf("custom mode commanded: %v, want refused", err)
	}

	b.handle(arlo.Connection{Up: false})
	if h.Online() {
		t.Error("online after the stream went down")
	}
	if got := h.Availability("BASE"); got != bridge.Unknown {
		t.Errorf("BASE offline bridge: %s, want unknown", got)
	}
}

// TestOpen opens a valid section, and refuses a misspelt key.
func TestOpen(t *testing.T) {
	secret := t.TempDir() + "/secret"
	if err := os.WriteFile(secret, []byte("pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := `{"email": "oiko@example.com", "passwordFile": "` + secret + `", "imapServer": "imap.example.com:993", "imapUser": "oiko@example.com", "imapPasswordFile": "` + secret + `"`
	if _, err := open(bridgetest.Env(t, "arlo", valid+`}`)); err != nil {
		t.Errorf("valid section: %v", err)
	}
	if _, err := open(bridgetest.Env(t, "arlo", valid+`, "dumpDirectory": "/tmp"}`)); err == nil {
		t.Error("misspelt key accepted")
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
	if len(cam.Functions) != 2 || cam.Functions[0].Key != "occupancy" || cam.Functions[0].Capabilities[0].Type != bridge.Binary ||
		cam.Functions[1].Key != "camera" || cam.Functions[1].Kind != "camera" || cam.Functions[1].Capabilities != nil {
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
		var c Config
		if err := (bridge.Env{Config: section.Config}).Decode(&c); err != nil {
			t.Errorf("type %s: example section: %v", typ, err)
		}
	}
}
