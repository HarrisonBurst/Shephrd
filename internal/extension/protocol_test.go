package extension

import (
	"strings"
	"testing"
)

func TestStrictDecodeRejectsUnknownDuplicateTrailingAndMalformedFrames(t *testing.T) {
	type value struct {
		Name string `json:"name"`
	}
	for _, test := range []struct {
		name  string
		frame string
	}{
		{name: "unknown", frame: `{"name":"valid","extra":true}`},
		{name: "duplicate", frame: `{"name":"first","name":"second"}`},
		{name: "trailing", frame: `{"name":"valid"} {}`},
		{name: "malformed", frame: `{"name":`},
		{name: "invalid utf8", frame: string([]byte{'{', '"', 'n', 'a', 'm', 'e', '"', ':', '"', 0xff, '"', '}'})},
	} {
		t.Run(test.name, func(t *testing.T) {
			var decoded value
			if err := StrictDecode([]byte(test.frame), &decoded); err == nil {
				t.Fatalf("frame was accepted: %q", test.frame)
			}
		})
	}
	oversized := []byte(`{"name":"` + strings.Repeat("x", MaxFrameBytes) + `"}`)
	var decoded value
	if err := StrictDecode(oversized, &decoded); err == nil {
		t.Fatal("oversized frame was accepted")
	}
}

func TestManifestRequiresExactWireIdentityCapabilitiesAndOperations(t *testing.T) {
	capability := Capability{Name: "terminal.cmux", Version: 1, Operations: []string{"detect", "inspect"}}
	valid := Manifest{Wire: WireRange{Major: WireMajor, MinorMin: WireMinor, MinorMax: WireMinor}, Extension: Identity{ID: "shephrd.terminal.cmux", Version: "1.0.0"}, Capabilities: []Capability{capability}}
	if err := ValidateManifest(valid, valid.Extension.ID, []Capability{capability}); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Manifest){
		func(manifest *Manifest) { manifest.Wire.Major++ },
		func(manifest *Manifest) { manifest.Extension.ID = "shephrd.terminal.other" },
		func(manifest *Manifest) { manifest.Capabilities[0].Version++ },
		func(manifest *Manifest) {
			manifest.Capabilities[0].Operations = append(manifest.Capabilities[0].Operations, "close")
		},
	} {
		candidate := valid
		candidate.Capabilities = []Capability{{Name: capability.Name, Version: capability.Version, Operations: append([]string(nil), capability.Operations...)}}
		mutate(&candidate)
		if err := ValidateManifest(candidate, valid.Extension.ID, []Capability{capability}); err == nil {
			t.Fatalf("manifest was accepted: %+v", candidate)
		}
	}
}
