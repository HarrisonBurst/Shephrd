package pibridge

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"shephrd/internal/adapter"
)

func TestChannelAcceptsOnlyExactMonotonicRunFrames(t *testing.T) {
	channel, err := Open("attempt_exact", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	info, err := os.Stat(channel.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %o", info.Mode().Perm())
	}
	go writeFrames(t, channel, []Frame{
		{SchemaVersion: SchemaVersion, Token: channel.Token, AttemptID: "attempt_exact", RunGeneration: 3, Sequence: 1, Kind: "session", SessionID: "session"},
		{SchemaVersion: SchemaVersion, Token: channel.Token, AttemptID: "attempt_exact", RunGeneration: 3, Sequence: 2, Kind: "event_candidate", Envelope: `<shephrd-event>{"type":"question","payload":"choice"}</shephrd-event>`},
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := channel.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	first, err := connection.Next()
	if err != nil || first.SessionID != "session" {
		t.Fatalf("first = %+v, err = %v", first, err)
	}
	if err := connection.Respond(first.Sequence, ResultAcceptedNonterminal, "", nil); err != nil {
		t.Fatal(err)
	}
	second, err := connection.Next()
	if err != nil || second.Kind != "event_candidate" {
		t.Fatalf("second = %+v, err = %v", second, err)
	}
	if err := connection.Respond(second.Sequence, ResultAcceptedTerminal, "", nil); err != nil {
		t.Fatal(err)
	}
}

func TestChannelResponsesAuthenticateAndCorrelateExactSequence(t *testing.T) {
	channel, err := Open("attempt_exact", 4)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	client, err := net.Dial("unix", channel.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := channel.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	frame := Frame{SchemaVersion: SchemaVersion, Token: channel.Token, AttemptID: channel.AttemptID, RunGeneration: channel.RunGeneration, Sequence: 1, Kind: "session", SessionID: "session"}
	if _, err := client.Write([]byte(encodeFrame(frame) + "\n")); err != nil {
		t.Fatal(err)
	}
	accepted, err := connection.Next()
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Respond(accepted.Sequence, ResultAcceptedNonterminal, "", nil); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(client).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatal(err)
	}
	if response.SchemaVersion != SchemaVersion || response.Token != channel.Token || response.AttemptID != channel.AttemptID || response.RunGeneration != channel.RunGeneration || response.AckSequence != frame.Sequence || response.Result != ResultAcceptedNonterminal {
		t.Fatalf("response=%+v", response)
	}
	candidate := Frame{SchemaVersion: SchemaVersion, Token: channel.Token, AttemptID: channel.AttemptID, RunGeneration: channel.RunGeneration, Sequence: 2, Kind: "event_candidate", Envelope: `<shephrd-event>{"type":"question","payload":"x","question":{}}</shephrd-event>`}
	if _, err := client.Write([]byte(encodeFrame(candidate) + "\n")); err != nil {
		t.Fatal(err)
	}
	accepted, err = connection.Next()
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := adapter.Diagnostic{Code: adapter.DiagnosticSchemaUnknownField, Phase: "adapter", Field: "question", Message: "unknown field"}
	if err := connection.Respond(accepted.Sequence, ResultRepair, "repair_exact", &diagnostic); err != nil {
		t.Fatal(err)
	}
	line, err = bufio.NewReader(client).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatal(err)
	}
	if response.AckSequence != 2 || response.Result != ResultRepair || response.RepairID != "repair_exact" || response.Diagnostic == nil || response.Diagnostic.Code != adapter.DiagnosticSchemaUnknownField {
		t.Fatalf("repair response=%+v", response)
	}
}

func TestChannelRejectsStaleSpoofedAndMalformedFrames(t *testing.T) {
	for _, test := range []struct {
		name   string
		line   func(*Channel) string
		reason string
	}{
		{"stale generation", func(channel *Channel) string {
			return encodeFrame(Frame{SchemaVersion: SchemaVersion, Token: channel.Token, AttemptID: channel.AttemptID, RunGeneration: 1, Sequence: 1, Kind: "session", SessionID: "session"})
		}, "stale"},
		{"spoofed token", func(channel *Channel) string {
			return encodeFrame(Frame{SchemaVersion: SchemaVersion, Token: "wrong", AttemptID: channel.AttemptID, RunGeneration: channel.RunGeneration, Sequence: 1, Kind: "session", SessionID: "session"})
		}, "token"},
		{"nonmonotonic sequence", func(channel *Channel) string {
			return encodeFrame(Frame{SchemaVersion: SchemaVersion, Token: channel.Token, AttemptID: channel.AttemptID, RunGeneration: channel.RunGeneration, Sequence: 2, Kind: "session", SessionID: "session"})
		}, "sequence"},
		{"unknown field", func(channel *Channel) string {
			return `{"schema_version":2,"token":"` + channel.Token + `","attempt_id":"` + channel.AttemptID + `","run_generation":2,"seq":1,"kind":"session","session_id":"session","extra":true}`
		}, "unknown field"},
	} {
		t.Run(test.name, func(t *testing.T) {
			channel, err := Open("attempt_exact", 2)
			if err != nil {
				t.Fatal(err)
			}
			defer channel.Close()
			go writeLine(t, channel.SocketPath, test.line(channel))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			connection, err := channel.Accept(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if _, err := connection.Next(); err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestWriteExtensionIsPrivateAndExact(t *testing.T) {
	path, err := WriteExtension(t.TempDir(), "attempt", 4)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !strings.Contains(string(body), "ctx.shutdown()") || !strings.Contains(string(body), `ctx.mode !== "tui"`) {
		t.Fatalf("extension mode=%o body=%q", info.Mode().Perm(), body)
	}
}

func writeFrames(t *testing.T, channel *Channel, frames []Frame) {
	t.Helper()
	conn, err := net.Dial("unix", channel.SocketPath)
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for _, frame := range frames {
		if _, err := conn.Write([]byte(encodeFrame(frame) + "\n")); err != nil {
			t.Error(err)
			return
		}
		if _, err := reader.ReadBytes('\n'); err != nil {
			t.Error(err)
			return
		}
	}
}

func encodeFrame(frame Frame) string {
	encoded, _ := json.Marshal(frame)
	return string(encoded)
}

func writeLine(t *testing.T, path, line string) {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		t.Error(err)
	}
}
