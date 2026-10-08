package scanner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	extensionhost "shephrd/internal/extension"
	"shephrd/internal/repository/discovery"
)

var requestIDPattern = regexp.MustCompile(`^extension_request_[0-9a-f]{24}$`)

func Run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("expected exactly one command")
	}
	switch args[0] {
	case "describe":
		return writeJSON(stdout, discovery.Manifest())
	case "invoke":
		return invoke(stdin, stdout)
	default:
		return fmt.Errorf("unknown command")
	}
}

func invoke(stdin io.Reader, stdout io.Writer) error {
	frame, err := readFrame(bufio.NewReaderSize(stdin, 64*1024))
	if err != nil {
		return err
	}
	var request extensionhost.Request
	if err := extensionhost.StrictDecode(frame, &request); err != nil {
		return err
	}
	if err := validateEnvelope(request); err != nil {
		return err
	}
	var payload discovery.Request
	if err := extensionhost.StrictDecode(request.Payload, &payload); err != nil {
		return writeError(stdout, request, "malformed", "repository discovery payload is invalid")
	}
	if err := validatePayload(payload); err != nil {
		return writeError(stdout, request, "malformed", err.Error())
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.UnixMilli(request.DeadlineUnixMS))
	defer cancel()
	candidates, err := scan(ctx, payload.Roots)
	if err != nil {
		return writeError(stdout, request, "unavailable", "repository discovery failed")
	}
	return writeResponse(stdout, request, discovery.Result{Candidates: candidates})
}

func validateEnvelope(request extensionhost.Request) error {
	now := time.Now()
	if request.Wire != (extensionhost.WireVersion{Major: extensionhost.WireMajor, Minor: extensionhost.WireMinor}) ||
		!requestIDPattern.MatchString(request.RequestID) || request.Capability != discovery.CapabilityName ||
		request.CapabilityVersion != discovery.CapabilityVersion || request.Operation != discovery.DiscoverOperation ||
		request.DeadlineUnixMS <= now.UnixMilli() || request.DeadlineUnixMS > now.Add(11*time.Second).UnixMilli() {
		return fmt.Errorf("request envelope is invalid or expired")
	}
	return nil
}

func validatePayload(payload discovery.Request) error {
	if err := discovery.ValidateRequest(payload); err != nil {
		return err
	}
	for _, root := range payload.Roots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root || strings.ContainsRune(root, 0) {
			return fmt.Errorf("repository discovery roots are invalid")
		}
		canonical, err := filepath.EvalSymlinks(root)
		if err == nil && filepath.Clean(canonical) != root || err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("repository discovery roots are not canonical")
		}
	}
	return nil
}

func scan(ctx context.Context, roots []string) ([]discovery.Candidate, error) {
	paths := make(map[string]bool)
	for _, root := range roots {
		if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				return walkErr
			}
			if path != root && entry.Name() == ".git" {
				candidate, err := filepath.EvalSymlinks(filepath.Dir(path))
				if err != nil {
					return err
				}
				candidate = filepath.Clean(candidate)
				if len(candidate) > discovery.MaxPathBytes {
					return fmt.Errorf("repository candidate path exceeds the size limit")
				}
				paths[candidate] = true
				if len(paths) > discovery.MaxCandidates {
					return fmt.Errorf("repository candidate count exceeds the limit")
				}
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !entry.IsDir() {
				return nil
			}
			if path != root {
				name := entry.Name()
				if name == "node_modules" || name == "vendor" || strings.HasPrefix(name, ".") && name != ".github" {
					return filepath.SkipDir
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	values := make([]string, 0, len(paths))
	for path := range paths {
		values = append(values, path)
	}
	sort.Strings(values)
	candidates := make([]discovery.Candidate, len(values))
	for index, path := range values {
		candidates[index] = discovery.Candidate{Path: path}
	}
	return candidates, nil
}

func writeResponse(writer io.Writer, request extensionhost.Request, result discovery.Result) error {
	payload, err := json.Marshal(result)
	if err != nil || len(payload) > extensionhost.MaxFrameBytes {
		return fmt.Errorf("encode extension result")
	}
	return writeJSON(writer, extensionhost.Response{
		Wire: request.Wire, RequestID: request.RequestID, Capability: request.Capability,
		CapabilityVersion: request.CapabilityVersion, Operation: request.Operation, Status: "ok",
		Result: payload, Extension: discovery.Manifest().Extension,
	})
}

func writeError(writer io.Writer, request extensionhost.Request, class, message string) error {
	if len(message) > 2048 {
		message = message[:2048]
	}
	return writeJSON(writer, extensionhost.Response{
		Wire: request.Wire, RequestID: request.RequestID, Capability: request.Capability,
		CapabilityVersion: request.CapabilityVersion, Operation: request.Operation, Status: "error",
		Error:     &extensionhost.Failure{Class: class, Code: "repository_discovery_failed", Message: message, Effect: "none"},
		Extension: discovery.Manifest().Extension,
	})
}

func writeJSON(writer io.Writer, value any) error {
	frame, err := json.Marshal(value)
	if err != nil || len(frame) > extensionhost.MaxFrameBytes {
		return fmt.Errorf("encode extension frame")
	}
	_, err = writer.Write(append(frame, '\n'))
	return err
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	var frame bytes.Buffer
	for {
		fragment, err := reader.ReadSlice('\n')
		if frame.Len()+len(fragment) > extensionhost.MaxFrameBytes {
			return nil, fmt.Errorf("extension request frame exceeds the size limit")
		}
		frame.Write(fragment)
		if err == nil {
			return bytes.TrimSuffix(frame.Bytes(), []byte{'\n'}), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && frame.Len() > 0 {
			return frame.Bytes(), nil
		}
		return nil, err
	}
}
