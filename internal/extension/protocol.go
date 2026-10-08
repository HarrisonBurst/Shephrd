package extension

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	WireMajor     = 1
	WireMinor     = 0
	MaxFrameBytes = 1024 * 1024
)

type WireRange struct {
	Major    int `json:"major"`
	MinorMin int `json:"minor_min"`
	MinorMax int `json:"minor_max"`
}

type WireVersion struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
}

type Identity struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	BuildCommit string `json:"build_commit,omitempty"`
}

type Capability struct {
	Name       string   `json:"name"`
	Version    int      `json:"version"`
	Operations []string `json:"operations"`
}

type Manifest struct {
	Wire         WireRange    `json:"wire"`
	Extension    Identity     `json:"extension"`
	Capabilities []Capability `json:"capabilities"`
}

type Request struct {
	Wire              WireVersion     `json:"wire"`
	RequestID         string          `json:"request_id"`
	Capability        string          `json:"capability"`
	CapabilityVersion int             `json:"capability_version"`
	Operation         string          `json:"operation"`
	DeadlineUnixMS    int64           `json:"deadline_unix_ms"`
	Payload           json.RawMessage `json:"payload"`
}

type Response struct {
	Wire              WireVersion     `json:"wire"`
	RequestID         string          `json:"request_id"`
	Capability        string          `json:"capability"`
	CapabilityVersion int             `json:"capability_version"`
	Operation         string          `json:"operation"`
	Status            string          `json:"status"`
	Result            json.RawMessage `json:"result,omitempty"`
	Error             *Failure        `json:"error,omitempty"`
	Extension         Identity        `json:"extension"`
}

type Failure struct {
	Class   string `json:"class"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Effect  string `json:"effect"`
}

type Control struct {
	Wire      WireVersion `json:"wire"`
	RequestID string      `json:"request_id"`
	Action    string      `json:"action"`
}

var identityPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$`)
var operationPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func ValidateManifest(manifest Manifest, expectedID string, expected []Capability) error {
	if manifest.Wire.Major != WireMajor || manifest.Wire.MinorMin > WireMinor || manifest.Wire.MinorMax < WireMinor || manifest.Wire.MinorMin < 0 || manifest.Wire.MinorMax < manifest.Wire.MinorMin {
		return fmt.Errorf("extension wire protocol is incompatible")
	}
	if manifest.Extension.ID != expectedID || !identityPattern.MatchString(manifest.Extension.ID) || invalidText(manifest.Extension.Version, 128, false) || invalidText(manifest.Extension.BuildCommit, 128, true) {
		return fmt.Errorf("extension identity is invalid")
	}
	if len(manifest.Capabilities) == 0 || len(manifest.Capabilities) > 16 {
		return fmt.Errorf("extension capability manifest is invalid")
	}
	if err := validateCapabilities(manifest.Capabilities); err != nil {
		return err
	}
	actual := canonicalCapabilities(manifest.Capabilities)
	wanted := canonicalCapabilities(expected)
	if !slices.EqualFunc(actual, wanted, func(a, b Capability) bool {
		return a.Name == b.Name && a.Version == b.Version && slices.Equal(a.Operations, b.Operations)
	}) {
		return fmt.Errorf("extension capability manifest does not exactly match the required capabilities")
	}
	return nil
}

func validateCapabilities(capabilities []Capability) error {
	seen := make(map[string]bool, len(capabilities))
	for _, capability := range capabilities {
		if !identityPattern.MatchString(capability.Name) || capability.Version <= 0 || capability.Version > 65535 || len(capability.Operations) == 0 || len(capability.Operations) > 64 || seen[capability.Name] {
			return fmt.Errorf("extension capability manifest is invalid")
		}
		seen[capability.Name] = true
		operations := make(map[string]bool, len(capability.Operations))
		for _, operation := range capability.Operations {
			if !operationPattern.MatchString(operation) || operations[operation] {
				return fmt.Errorf("extension capability operation is invalid")
			}
			operations[operation] = true
		}
	}
	return nil
}

func canonicalCapabilities(capabilities []Capability) []Capability {
	result := make([]Capability, len(capabilities))
	for index, capability := range capabilities {
		result[index] = capability
		result[index].Operations = append([]string(nil), capability.Operations...)
		slices.Sort(result[index].Operations)
	}
	slices.SortFunc(result, func(a, b Capability) int { return strings.Compare(a.Name, b.Name) })
	return result
}

func StrictDecode(data []byte, value any) error {
	if len(data) == 0 || len(data) > MaxFrameBytes || !utf8.Valid(data) {
		return fmt.Errorf("JSON frame is empty, oversized, or invalid UTF-8")
	}
	validator := json.NewDecoder(bytes.NewReader(data))
	validator.UseNumber()
	if err := validateJSONValue(validator); err != nil {
		return fmt.Errorf("invalid JSON frame: %w", err)
	}
	if token, err := validator.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("invalid JSON frame: trailing token %v", token)
		}
		return fmt.Errorf("invalid JSON frame: trailing data")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("invalid JSON frame: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("invalid JSON frame: trailing data")
	}
	return nil
}

func validateJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok || seen[key] {
				return fmt.Errorf("duplicate or invalid object field")
			}
			seen[key] = true
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return fmt.Errorf("unterminated object")
		}
	case '[':
		for decoder.More() {
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return fmt.Errorf("unterminated array")
		}
	default:
		return fmt.Errorf("unexpected delimiter")
	}
	return nil
}

func invalidText(value string, limit int, optional bool) bool {
	if value == "" {
		return !optional
	}
	return len(value) > limit || strings.IndexFunc(value, func(char rune) bool { return char < 0x20 || char == 0x7f }) >= 0
}
