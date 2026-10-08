package macos

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"runtime"
	"time"

	extensionhost "shephrd/internal/extension"
	"shephrd/internal/notification"
)

var requestIDPattern = regexp.MustCompile(`^extension_request_[0-9a-f]{24}$`)

type displayFunc func(context.Context, notification.Presentation) error

func Run(args []string, stdin io.Reader, stdout io.Writer) error {
	return run(args, stdin, stdout, displayNotification)
}

func run(args []string, stdin io.Reader, stdout io.Writer, display displayFunc) error {
	if len(args) != 1 {
		return fmt.Errorf("expected exactly one command")
	}
	switch args[0] {
	case "describe":
		return writeJSON(stdout, notification.Manifest())
	case "invoke":
		return invoke(stdin, stdout, display)
	default:
		return fmt.Errorf("unknown command")
	}
}

func invoke(stdin io.Reader, stdout io.Writer, display displayFunc) error {
	frame, err := readFrame(bufio.NewReaderSize(stdin, 64*1024))
	if err != nil {
		return err
	}
	var request extensionhost.Request
	if err := extensionhost.StrictDecode(frame, &request); err != nil {
		return err
	}
	if err := validateRequest(request); err != nil {
		return err
	}
	var presentation notification.Presentation
	if err := extensionhost.StrictDecode(request.Payload, &presentation); err != nil {
		return writeError(stdout, request, "malformed", "notification presentation payload is invalid")
	}
	if err := notification.ValidatePresentation(presentation); err != nil {
		return writeError(stdout, request, "malformed", err.Error())
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.UnixMilli(request.DeadlineUnixMS))
	defer cancel()
	if err := display(ctx, presentation); err != nil {
		return writeError(stdout, request, "unavailable", "macOS notification presentation failed")
	}
	return writeResponse(stdout, request, notification.PresentationResult{Presented: true})
}

func validateRequest(request extensionhost.Request) error {
	now := time.Now()
	if request.Wire != (extensionhost.WireVersion{Major: extensionhost.WireMajor, Minor: extensionhost.WireMinor}) ||
		!requestIDPattern.MatchString(request.RequestID) || request.Capability != notification.CapabilityName ||
		request.CapabilityVersion != notification.CapabilityVersion || request.Operation != notification.PresentationOperation ||
		request.DeadlineUnixMS <= now.UnixMilli() || request.DeadlineUnixMS > now.Add(5*time.Second).UnixMilli() {
		return fmt.Errorf("request envelope is invalid or expired")
	}
	return nil
}

func writeResponse(writer io.Writer, request extensionhost.Request, result notification.PresentationResult) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode extension result")
	}
	return writeJSON(writer, extensionhost.Response{
		Wire: request.Wire, RequestID: request.RequestID, Capability: request.Capability,
		CapabilityVersion: request.CapabilityVersion, Operation: request.Operation, Status: "ok",
		Result: payload, Extension: notification.Manifest().Extension,
	})
}

func writeError(writer io.Writer, request extensionhost.Request, class, message string) error {
	if len(message) > 2048 {
		message = message[:2048]
	}
	return writeJSON(writer, extensionhost.Response{
		Wire: request.Wire, RequestID: request.RequestID, Capability: request.Capability,
		CapabilityVersion: request.CapabilityVersion, Operation: request.Operation, Status: "error",
		Error:     &extensionhost.Failure{Class: class, Code: "notification_presentation_failed", Message: message, Effect: "none"},
		Extension: notification.Manifest().Extension,
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

func displayNotification(ctx context.Context, presentation notification.Presentation) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("macOS Notification Center is unavailable on %s", runtime.GOOS)
	}
	command := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", "on run argv\n\tdisplay notification (item 2 of argv) with title (item 1 of argv)\nend run", "--", presentation.Title, presentation.Body)
	if err := command.Run(); err != nil {
		return fmt.Errorf("run Notification Center presentation: %w", err)
	}
	return nil
}
