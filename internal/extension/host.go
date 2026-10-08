package extension

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"shephrd/internal/process"
)

const MaxDiagnosticBytes = 16 * 1024

const (
	ErrorDeadlineExceeded  = "deadline_exceeded"
	ErrorCancelled         = "cancelled"
	ErrorExtensionExit     = "extension_exit"
	ErrorProtocolViolation = "protocol_violation"
	ErrorAbortUncertain    = "abort_uncertain"
)

type HostError struct {
	Kind      string
	Operation string
	Cause     error
}

func (e *HostError) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("extension %s: %s", e.Operation, e.Kind)
	}
	return fmt.Sprintf("extension %s: %s: %v", e.Operation, e.Kind, e.Cause)
}

func (e *HostError) Unwrap() error {
	return e.Cause
}

func (e *HostError) ErrorKind() string {
	return "extension_" + e.Kind
}

type RemoteError struct {
	Failure   Failure
	Operation string
}

func (e *RemoteError) Error() string {
	if e.Failure.Message == "" {
		return fmt.Sprintf("extension %s: %s", e.Operation, e.Failure.Code)
	}
	return fmt.Sprintf("extension %s: %s: %s", e.Operation, e.Failure.Code, e.Failure.Message)
}

type HostConfig struct {
	Command              []string
	SHA256               string
	ParentEnvironment    []string
	AllowedEnvironment   []string
	ExpectedID           string
	ExpectedCapabilities []Capability
	RequestID            func() (string, error)
	AbortTimeout         time.Duration
	describeTimeout      time.Duration
}

type Host struct {
	config        HostConfig
	manifestMu    sync.Mutex
	manifest      Manifest
	manifestReady bool
	redactor      *redactor
}

func NewHost(config HostConfig) *Host {
	config.Command = append([]string(nil), config.Command...)
	config.ParentEnvironment = filterEnvironment(config.ParentEnvironment, config.AllowedEnvironment...)
	config.AllowedEnvironment = nil
	config.ExpectedCapabilities = canonicalCapabilities(config.ExpectedCapabilities)
	if config.RequestID == nil {
		config.RequestID = newRequestID
	}
	if config.describeTimeout <= 0 {
		config.describeTimeout = 5 * time.Second
	}
	if config.AbortTimeout <= 0 {
		config.AbortTimeout = 15 * time.Second
	}
	return &Host{config: config, redactor: newRedactor(config.ParentEnvironment)}
}

func filterEnvironment(environment []string, allowed ...string) []string {
	values := make(map[string]string)
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if found {
			values[key] = value
		}
	}
	result := make([]string, 0, len(allowed))
	for _, key := range allowed {
		if value, exists := values[key]; exists && !strings.ContainsRune(value, 0) && len(value) <= 16384 {
			result = append(result, key+"="+value)
		}
	}
	return result
}

func (h *Host) Describe(ctx context.Context) (Manifest, error) {
	h.manifestMu.Lock()
	defer h.manifestMu.Unlock()
	if h.manifestReady {
		return h.manifest, nil
	}
	manifest, err := h.describe(ctx)
	if err != nil {
		return Manifest{}, err
	}
	h.manifest = manifest
	h.manifestReady = true
	return h.manifest, nil
}

func (h *Host) describe(ctx context.Context) (Manifest, error) {
	describeContext, cancel := context.WithTimeout(ctx, h.config.describeTimeout)
	defer cancel()
	output, diagnostic, err := h.runOne(describeContext, "describe", nil)
	if err != nil {
		return Manifest{}, h.executionError(describeContext, "describe", err, diagnostic)
	}
	var manifest Manifest
	if err := StrictDecode(output, &manifest); err != nil {
		return Manifest{}, &HostError{Kind: ErrorProtocolViolation, Operation: "describe", Cause: err}
	}
	if err := ValidateManifest(manifest, h.config.ExpectedID, h.config.ExpectedCapabilities); err != nil {
		return Manifest{}, &HostError{Kind: ErrorProtocolViolation, Operation: "describe", Cause: err}
	}
	return manifest, nil
}

func (h *Host) Invoke(ctx context.Context, capability string, version int, operation string, payload, result any) error {
	invocation, err := h.Begin(ctx, capability, version, operation, payload)
	if err != nil {
		return err
	}
	if _, err := invocation.Receive(result, "ok"); err != nil {
		if cancelErr := invocation.Cancel(); cancelErr != nil {
			return errors.Join(err, fmt.Errorf("terminate extension process tree: %w", cancelErr))
		}
		return err
	}
	return invocation.Finish()
}

func (h *Host) Begin(ctx context.Context, capability string, version int, operation string, payload any) (*Invocation, error) {
	manifest, err := h.Describe(ctx)
	if err != nil {
		return nil, err
	}
	if !h.supports(capability, version, operation) {
		return nil, &HostError{Kind: ErrorProtocolViolation, Operation: operation, Cause: fmt.Errorf("capability operation was not declared")}
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, &HostError{Kind: ErrorProtocolViolation, Operation: operation, Cause: fmt.Errorf("extension invocation requires a deadline")}
	}
	requestID, err := h.config.RequestID()
	if err != nil {
		return nil, fmt.Errorf("create extension request identity: %w", err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil || len(payloadJSON) > MaxFrameBytes {
		return nil, &HostError{Kind: ErrorProtocolViolation, Operation: operation, Cause: fmt.Errorf("request payload is invalid or oversized")}
	}
	request := Request{Wire: WireVersion{Major: WireMajor, Minor: WireMinor}, RequestID: requestID, Capability: capability, CapabilityVersion: version, Operation: operation, DeadlineUnixMS: deadline.UnixMilli(), Payload: payloadJSON}
	session, err := h.start(ctx, "invoke")
	if err != nil {
		return nil, h.executionError(ctx, operation, err, "")
	}
	invocation := &Invocation{host: h, session: session, request: request, identity: manifest.Extension}
	if err := session.send(request); err != nil {
		_ = invocation.Cancel()
		return nil, h.sessionError(ctx, operation, err, session)
	}
	return invocation, nil
}

func (h *Host) supports(name string, version int, operation string) bool {
	for _, capability := range h.config.ExpectedCapabilities {
		if capability.Name == name && capability.Version == version {
			for _, candidate := range capability.Operations {
				if candidate == operation {
					return true
				}
			}
		}
	}
	return false
}

type Invocation struct {
	host     *Host
	session  *processSession
	request  Request
	identity Identity
	finished bool
}

func (i *Invocation) Receive(result any, statuses ...string) (string, error) {
	if i.finished {
		return "", &HostError{Kind: ErrorProtocolViolation, Operation: i.request.Operation, Cause: fmt.Errorf("invocation is already finished")}
	}
	frame, err := i.session.receive()
	if err != nil {
		return "", i.host.sessionError(i.session.ctx, i.request.Operation, err, i.session)
	}
	var response Response
	if err := StrictDecode(frame, &response); err != nil {
		return "", &HostError{Kind: ErrorProtocolViolation, Operation: i.request.Operation, Cause: err}
	}
	if response.Wire != i.request.Wire || response.RequestID != i.request.RequestID || response.Capability != i.request.Capability || response.CapabilityVersion != i.request.CapabilityVersion || response.Operation != i.request.Operation || response.Extension != i.identity {
		return "", &HostError{Kind: ErrorProtocolViolation, Operation: i.request.Operation, Cause: fmt.Errorf("response identity did not exactly match the request and manifest")}
	}
	if response.Status == "error" {
		if response.Error == nil || len(response.Result) != 0 || !validFailure(*response.Error) {
			return "", &HostError{Kind: ErrorProtocolViolation, Operation: i.request.Operation, Cause: fmt.Errorf("extension error envelope is invalid")}
		}
		response.Error.Message = i.host.redactor.redact(response.Error.Message)
		return "", &RemoteError{Failure: *response.Error, Operation: i.request.Operation}
	}
	if response.Error != nil || len(response.Result) == 0 || !contains(statuses, response.Status) {
		return "", &HostError{Kind: ErrorProtocolViolation, Operation: i.request.Operation, Cause: fmt.Errorf("extension response status is invalid")}
	}
	if err := StrictDecode(response.Result, result); err != nil {
		return "", &HostError{Kind: ErrorProtocolViolation, Operation: i.request.Operation, Cause: err}
	}
	return response.Status, nil
}

func (i *Invocation) SendControl(action string) error {
	if i.finished || action != "commit" && action != "abort" {
		return &HostError{Kind: ErrorProtocolViolation, Operation: i.request.Operation, Cause: fmt.Errorf("extension control action is invalid")}
	}
	control := Control{Wire: i.request.Wire, RequestID: i.request.RequestID, Action: action}
	if err := i.session.send(control); err != nil {
		return i.host.sessionError(i.session.ctx, i.request.Operation, err, i.session)
	}
	return nil
}

func (i *Invocation) Finish() error {
	if i.finished {
		return nil
	}
	i.finished = true
	if err := i.session.finish(); err != nil {
		return i.host.sessionError(i.session.ctx, i.request.Operation, err, i.session)
	}
	return nil
}

func (i *Invocation) Cancel() error {
	if i.finished {
		return nil
	}
	i.finished = true
	return i.session.stop()
}

// Abort explicitly requests that the extension clean up the in-flight
// invocation and waits for the extension to prove the outcome within the
// host abort bound. A nil result means the extension acknowledged that no
// side effect remains. Any error means the cleanup outcome is uncertain and
// the caller must retain recoverable state.
func (i *Invocation) Abort() error {
	if i.finished {
		return nil
	}
	abortContext, cancel := context.WithTimeout(context.Background(), i.host.config.AbortTimeout)
	defer cancel()
	go func() {
		select {
		case <-abortContext.Done():
			_ = i.session.stop()
		case <-i.session.waitDone:
		}
	}()
	if err := i.SendControl("abort"); err != nil {
		i.finished = true
		return i.host.abortUncertain(i.request.Operation, i.session, err)
	}
	for {
		frame, err := i.session.receive()
		if err != nil {
			i.finished = true
			if abortContext.Err() != nil {
				return i.host.abortUncertain(i.request.Operation, i.session, errors.Join(fmt.Errorf("wait for explicit abort acknowledgement"), err))
			}
			return i.host.sessionError(abortContext, i.request.Operation, err, i.session)
		}
		var response Response
		if err := StrictDecode(frame, &response); err != nil {
			i.finished = true
			_ = i.session.stop()
			return &HostError{Kind: ErrorProtocolViolation, Operation: i.request.Operation, Cause: err}
		}
		if response.Wire != i.request.Wire || response.RequestID != i.request.RequestID || response.Capability != i.request.Capability || response.CapabilityVersion != i.request.CapabilityVersion || response.Operation != i.request.Operation || response.Extension != i.identity {
			i.finished = true
			_ = i.session.stop()
			return &HostError{Kind: ErrorProtocolViolation, Operation: i.request.Operation, Cause: fmt.Errorf("response identity did not exactly match the request and manifest")}
		}
		switch response.Status {
		case "prepared":
			continue
		case "aborted":
			i.finished = true
			return i.session.finish()
		case "error":
			if response.Error == nil || len(response.Result) != 0 || !validFailure(*response.Error) {
				i.finished = true
				_ = i.session.stop()
				return &HostError{Kind: ErrorProtocolViolation, Operation: i.request.Operation, Cause: fmt.Errorf("extension error envelope is invalid")}
			}
			response.Error.Message = i.host.redactor.redact(response.Error.Message)
			i.finished = true
			if response.Error.Effect == "none" {
				return i.session.finish()
			}
			stopErr := i.session.stop()
			return errors.Join(&RemoteError{Failure: *response.Error, Operation: i.request.Operation}, stopErr)
		default:
			i.finished = true
			_ = i.session.stop()
			return &HostError{Kind: ErrorProtocolViolation, Operation: i.request.Operation, Cause: fmt.Errorf("extension response status is invalid")}
		}
	}
}

func (h *Host) abortUncertain(operation string, session *processSession, cause error) error {
	return &HostError{Kind: ErrorAbortUncertain, Operation: operation, Cause: errors.Join(cause, session.stop())}
}

func validFailure(failure Failure) bool {
	if invalidText(failure.Class, 64, false) || invalidText(failure.Code, 128, false) || invalidText(failure.Message, 2048, false) {
		return false
	}
	switch failure.Effect {
	case "none", "known", "unknown":
		return true
	default:
		return false
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func newRequestID() (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "extension_request_" + hex.EncodeToString(value), nil
}

func (h *Host) executionError(ctx context.Context, operation string, err error, diagnostic string) error {
	kind := ErrorExtensionExit
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		kind = ErrorDeadlineExceeded
	} else if errors.Is(ctx.Err(), context.Canceled) {
		kind = ErrorCancelled
	}
	message := h.redactor.redact(diagnostic)
	if message == "" {
		message = h.redactor.redact(err.Error())
	}
	return &HostError{Kind: kind, Operation: operation, Cause: errors.New(message)}
}

func (h *Host) sessionError(ctx context.Context, operation string, err error, session *processSession) error {
	return h.executionError(ctx, operation, err, session.diagnostic())
}

func (h *Host) runOne(ctx context.Context, mode string, input []byte) ([]byte, string, error) {
	if err := h.validateExecutable(); err != nil {
		return nil, "", err
	}
	args := append(append([]string(nil), h.config.Command[1:]...), mode)
	cmd := exec.Command(h.config.Command[0], args...)
	cmd.Env = h.config.ParentEnvironment
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	stdout := &limitedBuffer{limit: MaxFrameBytes}
	stderr := &limitedBuffer{limit: MaxDiagnosticBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	process.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	pid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if stopErr := process.Stop(pid); err == nil {
			err = stopErr
		}
		done <- err
	}()
	select {
	case err := <-done:
		if stdout.Exceeded() || stderr.Exceeded() {
			return nil, stderr.String(), fmt.Errorf("extension output exceeded its bound")
		}
		return stdout.Bytes(), stderr.String(), err
	case <-ctx.Done():
		_ = process.Stop(pid)
		err := <-done
		if err == nil {
			err = ctx.Err()
		}
		return nil, stderr.String(), err
	}
}

func (h *Host) start(ctx context.Context, mode string) (*processSession, error) {
	if err := h.validateExecutable(); err != nil {
		return nil, err
	}
	args := append(append([]string(nil), h.config.Command[1:]...), mode)
	cmd := exec.Command(h.config.Command[0], args...)
	cmd.Env = h.config.ParentEnvironment
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	defer stdoutWriter.Close()
	cmd.Stdout = stdoutWriter
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	process.Detach(cmd)
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		return nil, err
	}
	session := &processSession{ctx: ctx, cmd: cmd, stdin: stdin, stdout: bufio.NewReaderSize(stdout, 64*1024), stdoutPipe: stdout, stderr: &limitedBuffer{limit: MaxDiagnosticBytes}, waitDone: make(chan struct{}), shutdownDone: make(chan struct{})}
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(session.stderr, stderr)
		close(stderrDone)
	}()
	go func() {
		session.waitErr = cmd.Wait()
		session.treeErr = process.Stop(cmd.Process.Pid)
		<-stderrDone
		close(session.waitDone)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = session.stop()
		case <-session.waitDone:
		}
	}()
	return session, nil
}

func (h *Host) validateExecutable() error {
	if len(h.config.Command) == 0 || len(h.config.Command) > 16 || !filepath.IsAbs(h.config.Command[0]) {
		return fmt.Errorf("extension command must name an absolute executable")
	}
	for _, argument := range h.config.Command {
		if argument == "" || len(argument) > 8192 || strings.ContainsRune(argument, 0) {
			return fmt.Errorf("extension command contains an invalid argument")
		}
	}
	if len(h.config.SHA256) != 64 || strings.ToLower(h.config.SHA256) != h.config.SHA256 {
		return fmt.Errorf("extension SHA-256 is invalid")
	}
	if _, err := hex.DecodeString(h.config.SHA256); err != nil {
		return fmt.Errorf("extension SHA-256 is invalid")
	}
	info, err := os.Lstat(h.config.Command[0])
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("extension executable is missing, unsafe, or not executable")
	}
	file, err := os.Open(h.config.Command[0])
	if err != nil {
		return fmt.Errorf("open extension executable: %w", err)
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return fmt.Errorf("hash extension executable")
	}
	if hex.EncodeToString(digest.Sum(nil)) != h.config.SHA256 {
		return fmt.Errorf("extension executable SHA-256 does not match trusted configuration")
	}
	return nil
}

type processSession struct {
	ctx          context.Context
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	stdout       *bufio.Reader
	stdoutPipe   *os.File
	stderr       *limitedBuffer
	waitDone     chan struct{}
	waitErr      error
	treeErr      error
	stopErr      error
	closeOnce    sync.Once
	shutdownOnce sync.Once
	shutdownDone chan struct{}
}

func (s *processSession) send(value any) error {
	frame, err := json.Marshal(value)
	if err != nil || len(frame) > MaxFrameBytes {
		return fmt.Errorf("extension request frame is invalid or oversized")
	}
	frame = append(frame, '\n')
	_, err = s.stdin.Write(frame)
	return err
}

func (s *processSession) receive() ([]byte, error) {
	var frame bytes.Buffer
	for {
		fragment, err := s.stdout.ReadSlice('\n')
		if frame.Len()+len(fragment) > MaxFrameBytes {
			return nil, fmt.Errorf("extension response frame exceeds the size limit")
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

func (s *processSession) finish() error {
	defer s.stdoutPipe.Close()
	s.closeInput()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	readDone := make(chan error, 1)
	go func() {
		trailing, err := io.ReadAll(io.LimitReader(s.stdout, MaxFrameBytes+1))
		if err != nil || len(trailing) > MaxFrameBytes || len(bytes.TrimSpace(trailing)) != 0 {
			readDone <- fmt.Errorf("extension wrote unexpected protocol output")
			return
		}
		readDone <- nil
	}()
	waitDone := s.waitDone
	for readDone != nil || waitDone != nil {
		select {
		case err := <-readDone:
			if err != nil {
				return errors.Join(err, s.stop())
			}
			readDone = nil
		case <-waitDone:
			waitDone = nil
		case <-s.ctx.Done():
			return errors.Join(s.ctx.Err(), s.stop())
		case <-timer.C:
			return errors.Join(fmt.Errorf("extension did not finish within its bound"), s.stop())
		}
	}
	if s.waitErr != nil {
		return s.waitErr
	}
	if s.treeErr != nil {
		return s.treeErr
	}
	if s.stderr.Exceeded() {
		return fmt.Errorf("extension diagnostics exceeded the size limit")
	}
	return nil
}

func (s *processSession) stop() error {
	s.shutdownOnce.Do(func() {
		defer close(s.shutdownDone)
		defer s.stdoutPipe.Close()
		s.closeInput()
		select {
		case <-s.waitDone:
			return
		case <-time.After(2 * time.Second):
		}
		if s.cmd != nil && s.cmd.Process != nil {
			s.stopErr = process.Stop(s.cmd.Process.Pid)
		}
		<-s.waitDone
	})
	<-s.shutdownDone
	return errors.Join(s.stopErr, s.treeErr)
}

func (s *processSession) closeInput() {
	s.closeOnce.Do(func() { _ = s.stdin.Close() })
}

func (s *processSession) diagnostic() string {
	return s.stderr.String()
}

type limitedBuffer struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	available := b.limit - b.buffer.Len()
	if available > 0 {
		if len(data) < available {
			available = len(data)
		}
		_, _ = b.buffer.Write(data[:available])
	}
	if available < len(data) {
		b.exceeded = true
	}
	return len(data), nil
}

func (b *limitedBuffer) Exceeded() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exceeded
}

func (b *limitedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buffer.Bytes()...)
}

func (b *limitedBuffer) String() string {
	return strings.ToValidUTF8(string(b.Bytes()), "")
}

type redactor struct {
	values      []string
	assignments []*regexp.Regexp
}

func newRedactor(environment []string) *redactor {
	result := &redactor{}
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if !found || !sensitiveKey(key) {
			continue
		}
		if value != "" {
			result.values = append(result.values, value)
		}
		result.assignments = append(result.assignments, regexp.MustCompile(regexp.QuoteMeta(key)+`=[^\s]*`))
	}
	return result
}

func (r *redactor) redact(value string) string {
	if len(value) > MaxDiagnosticBytes {
		value = value[:MaxDiagnosticBytes]
	}
	value = strings.ToValidUTF8(value, "")
	for _, secret := range r.values {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	for _, assignment := range r.assignments {
		value = assignment.ReplaceAllString(value, "[REDACTED]")
	}
	return strings.TrimSpace(value)
}

func sensitiveKey(key string) bool {
	key = strings.ToUpper(key)
	return strings.Contains(key, "PASSWORD") || strings.Contains(key, "TOKEN") || strings.Contains(key, "SECRET") || strings.Contains(key, "CAPABILITY") || strings.Contains(key, "CREDENTIAL")
}
