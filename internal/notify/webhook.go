package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"shephrd/internal/config"
)

const maxSecret = 4 << 10

// Webhook posts one inbox item, signed in the Standard Webhooks style:
// webhook-signature is v1,base64(HMAC-SHA256(id.timestamp.body)).
func Webhook(ctx context.Context, d config.Delivery, id string, body []byte, now time.Time) (string, string) {
	key, err := readSecret(d.SecretFile)
	if err != nil {
		return "rejected", err.Error()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return "rejected", "invalid url: " + err.Error()
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("webhook-id", id)
	request.Header.Set("webhook-timestamp", timestamp)
	request.Header.Set("webhook-signature", Signature(key, id, timestamp, body))
	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(request)
	if err != nil {
		return "retryable", "receiver unreachable"
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	response.Body.Close()
	detail := "HTTP " + strconv.Itoa(response.StatusCode)
	switch status := response.StatusCode; {
	case status >= 200 && status < 300:
		return "delivered", detail
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500:
		return "retryable", detail
	}
	return "rejected", detail
}

func Signature(key []byte, id, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// readSecret accepts only a private file owned by this user. A whsec_
// prefix marks base64 key material.
func readSecret(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() == 0 || info.Size() > maxSecret {
		return nil, fmt.Errorf("secret_file must be a nonempty regular file with mode 0600 or stricter")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		return nil, fmt.Errorf("secret_file must be owned by the current user")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	secret := strings.TrimSpace(string(body))
	if encoded, ok := strings.CutPrefix(secret, "whsec_"); ok {
		return base64.StdEncoding.DecodeString(encoded)
	}
	return []byte(secret), nil
}
