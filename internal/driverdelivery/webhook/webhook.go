package webhook

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"shephrd/internal/driverdelivery"
	extensionhost "shephrd/internal/extension"
)

const (
	ExtensionID       = "shephrd.delivery-webhook"
	ExtensionVersion  = "1.0.0"
	MaxBodyBytes      = 64 * 1024
	MaxResponseBytes  = 4 * 1024
	MaxSecretBytes    = 4 * 1024
	MaxRequestTimeout = 60 * time.Second
	responseMargin    = 500 * time.Millisecond
)

var requestIDPattern = regexp.MustCompile(`^extension_request_[0-9a-f]{24}$`)

type settings struct {
	endpoint *url.URL
	key      []byte
}

func Run(args []string, stdin io.Reader, stdout io.Writer) error {
	return run(args, stdin, stdout, time.Now)
}

func run(args []string, stdin io.Reader, stdout io.Writer, now func() time.Time) error {
	if len(args) == 0 {
		return fmt.Errorf("expected a command")
	}
	configured, err := parseSettings(args[:len(args)-1])
	if err != nil {
		return err
	}
	switch args[len(args)-1] {
	case "describe":
		return writeJSON(stdout, Manifest())
	case "invoke":
		return invoke(configured, stdin, stdout, now)
	default:
		return fmt.Errorf("unknown command")
	}
}

func Manifest() extensionhost.Manifest {
	return extensionhost.Manifest{
		Wire:         extensionhost.WireRange{Major: extensionhost.WireMajor, MinorMin: extensionhost.WireMinor, MinorMax: extensionhost.WireMinor},
		Extension:    extensionhost.Identity{ID: ExtensionID, Version: ExtensionVersion, BuildCommit: buildCommit()},
		Capabilities: []extensionhost.Capability{driverdelivery.Capability()},
	}
}

func parseSettings(args []string) (settings, error) {
	flags := flag.NewFlagSet("shephrd-delivery-webhook", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	endpoint := flags.String("url", "", "Receiver URL")
	secretFile := flags.String("secret-file", "", "Absolute path to the signing secret")
	allowHTTP := flags.Bool("allow-http", false, "Permit a plain HTTP receiver URL")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return settings{}, fmt.Errorf("usage: shephrd-delivery-webhook --url <url> --secret-file <path> [--allow-http] describe|invoke")
	}
	parsed, err := url.Parse(*endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
		return settings{}, fmt.Errorf("--url must be an absolute URL without credentials or fragment")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && *allowHTTP) {
		return settings{}, fmt.Errorf("--url must use https unless --allow-http is configured")
	}
	key, err := readSecret(*secretFile)
	if err != nil {
		return settings{}, err
	}
	return settings{endpoint: parsed, key: key}, nil
}

func readSecret(path string) ([]byte, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("--secret-file must be an absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() == 0 || info.Size() > MaxSecretBytes {
		return nil, fmt.Errorf("secret file must be a nonempty regular file with mode 0600 or stricter")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		return nil, fmt.Errorf("secret file must be owned by the current user")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read secret file")
	}
	secret := strings.TrimSpace(string(body))
	if encoded, ok := strings.CutPrefix(secret, "whsec_"); ok {
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(key) == 0 {
			return nil, fmt.Errorf("whsec_ secret must contain base64 key material")
		}
		return key, nil
	}
	if secret == "" {
		return nil, fmt.Errorf("secret file is empty")
	}
	return []byte(secret), nil
}

func invoke(configured settings, stdin io.Reader, stdout io.Writer, now func() time.Time) error {
	frame, err := readFrame(bufio.NewReaderSize(stdin, 64*1024))
	if err != nil {
		return err
	}
	var request extensionhost.Request
	if err := extensionhost.StrictDecode(frame, &request); err != nil {
		return err
	}
	if err := validateEnvelope(request, now()); err != nil {
		return err
	}
	var delivery driverdelivery.Request
	if err := extensionhost.StrictDecode(request.Payload, &delivery); err != nil {
		return writeError(stdout, request, "delivery request payload is invalid")
	}
	if err := driverdelivery.ValidateRequest(delivery); err != nil {
		return writeError(stdout, request, err.Error())
	}
	body, err := json.Marshal(delivery)
	if err != nil || len(body) > MaxBodyBytes {
		return writeError(stdout, request, "delivery request body is oversized")
	}
	deadline := time.UnixMilli(request.DeadlineUnixMS)
	if margin := deadline.Add(-responseMargin); margin.After(now()) {
		deadline = margin
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	id := WebhookID(delivery.Notification.NotificationID, delivery.Claim.ClaimToken)
	timestamp := strconv.FormatInt(now().Unix(), 10)
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, configured.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return writeError(stdout, request, "delivery request could not be constructed")
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("User-Agent", ExtensionID+"/"+ExtensionVersion)
	httpRequest.Header.Set("webhook-id", id)
	httpRequest.Header.Set("webhook-timestamp", timestamp)
	httpRequest.Header.Set("webhook-signature", Signature(configured.key, id, timestamp, body))
	return writeResponse(stdout, request, deliver(ctx, httpRequest))
}

func deliver(ctx context.Context, request *http.Request) driverdelivery.Result {
	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true, MaxResponseHeaderBytes: MaxResponseBytes * 4},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return driverdelivery.Result{Outcome: driverdelivery.OutcomeRetryable, Detail: "receiver timed out"}
		}
		return driverdelivery.Result{Outcome: driverdelivery.OutcomeRetryable, Detail: "receiver connection failed"}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, MaxResponseBytes))
	_ = response.Body.Close()
	return Outcome(response.StatusCode)
}

func Outcome(status int) driverdelivery.Result {
	detail := "HTTP " + strconv.Itoa(status)
	switch {
	case status >= 200 && status < 300:
		return driverdelivery.Result{Outcome: driverdelivery.OutcomeDelivered, Detail: detail}
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 && status < 600:
		return driverdelivery.Result{Outcome: driverdelivery.OutcomeRetryable, Detail: detail}
	default:
		return driverdelivery.Result{Outcome: driverdelivery.OutcomeRejected, Detail: detail}
	}
}

func WebhookID(notificationID, claimToken string) string {
	digest := sha256.Sum256([]byte(notificationID + ":" + claimToken))
	return hex.EncodeToString(digest[:])
}

func Signature(key []byte, id, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func validateEnvelope(request extensionhost.Request, now time.Time) error {
	if request.Wire != (extensionhost.WireVersion{Major: extensionhost.WireMajor, Minor: extensionhost.WireMinor}) ||
		!requestIDPattern.MatchString(request.RequestID) || request.Capability != driverdelivery.CapabilityName ||
		request.CapabilityVersion != driverdelivery.CapabilityVersion || request.Operation != driverdelivery.DeliverOperation ||
		request.DeadlineUnixMS <= now.UnixMilli() || request.DeadlineUnixMS > now.Add(MaxRequestTimeout).UnixMilli() {
		return fmt.Errorf("request envelope is invalid or expired")
	}
	return nil
}

func writeResponse(writer io.Writer, request extensionhost.Request, result driverdelivery.Result) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode extension result")
	}
	return writeJSON(writer, extensionhost.Response{
		Wire: request.Wire, RequestID: request.RequestID, Capability: request.Capability,
		CapabilityVersion: request.CapabilityVersion, Operation: request.Operation, Status: "ok",
		Result: payload, Extension: Manifest().Extension,
	})
}

func writeError(writer io.Writer, request extensionhost.Request, message string) error {
	return writeJSON(writer, extensionhost.Response{
		Wire: request.Wire, RequestID: request.RequestID, Capability: request.Capability,
		CapabilityVersion: request.CapabilityVersion, Operation: request.Operation, Status: "error",
		Error:     &extensionhost.Failure{Class: "malformed", Code: "driver_delivery_invalid", Message: message, Effect: "none"},
		Extension: Manifest().Extension,
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

func buildCommit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && len(setting.Value) <= 128 {
			return setting.Value
		}
	}
	return ""
}
