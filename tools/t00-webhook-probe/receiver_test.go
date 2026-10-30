package webhookprobe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

const fixtureSecret = "fixture-only-not-a-secret"

type replayCache map[string]struct{}

func verifyServiceHook(
	headers http.Header,
	body []byte,
	secret []byte,
	now time.Time,
	maxSkew time.Duration,
	seen replayCache,
) int {
	signature := headers.Get("X-ServiceHook-Signature")
	timestampValue := headers.Get("X-ServiceHook-Timestamp")
	guid := headers.Get("X-ServiceHook-GUID")
	if signature == "" || timestampValue == "" || guid == "" {
		return http.StatusUnauthorized
	}

	timestamp, err := strconv.ParseInt(timestampValue, 10, 64)
	if err != nil {
		return http.StatusUnauthorized
	}
	delta := now.Sub(time.Unix(timestamp, 0))
	if delta < -maxSkew || delta > maxSkew {
		return http.StatusUnauthorized
	}

	provided, err := hex.DecodeString(signature)
	if err != nil || len(provided) != sha256.Size {
		return http.StatusUnauthorized
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), provided) {
		return http.StatusUnauthorized
	}

	replayKey := guid + ":" + timestampValue + ":" + signature
	if _, exists := seen[replayKey]; exists {
		return http.StatusConflict
	}
	seen[replayKey] = struct{}{}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return http.StatusBadRequest
	}
	return http.StatusNoContent
}

func TestServiceHookReceiverContract(t *testing.T) {
	repoRoot := os.Getenv("REPO_ROOT")
	if repoRoot == "" {
		t.Fatal("REPO_ROOT is required")
	}

	for _, version := range []string{"sentry-24.7.1", "sentry-26.10.1"} {
		t.Run(version, func(t *testing.T) {
			fixtureDir := filepath.Join(repoRoot, "testdata", "http-fixtures", version)
			body := readFixture(t, filepath.Join(fixtureDir, "service-hook-event-alert.body.json"))
			headersJSON := readFixture(t, filepath.Join(fixtureDir, "service-hook-event-alert.headers.json"))
			var fixtureHeaders map[string]string
			if err := json.Unmarshal(headersJSON, &fixtureHeaders); err != nil {
				t.Fatalf("decode headers: %v", err)
			}
			headers := make(http.Header)
			for name, value := range fixtureHeaders {
				headers.Set(name, value)
			}
			timestamp, err := strconv.ParseInt(headers.Get("X-ServiceHook-Timestamp"), 10, 64)
			if err != nil {
				t.Fatalf("parse fixture timestamp: %v", err)
			}
			now := time.Unix(timestamp, 0)
			maxSkew := 5 * time.Minute

			if status := verifyServiceHook(headers, body, []byte(fixtureSecret), now, maxSkew, replayCache{}); status != http.StatusNoContent {
				t.Fatalf("valid signed fixture: got status %d", status)
			}

			unsigned := headers.Clone()
			unsigned.Del("X-ServiceHook-Signature")
			if status := verifyServiceHook(unsigned, []byte("{"), []byte(fixtureSecret), now, maxSkew, replayCache{}); status != http.StatusUnauthorized {
				t.Fatalf("unsigned malformed JSON must fail authentication first: got status %d", status)
			}

			tamperedBody := append([]byte(nil), body...)
			tamperedBody[len(tamperedBody)-1] ^= 1
			if status := verifyServiceHook(headers, tamperedBody, []byte(fixtureSecret), now, maxSkew, replayCache{}); status != http.StatusUnauthorized {
				t.Fatalf("tampered body: got status %d", status)
			}

			staleNow := now.Add(maxSkew + time.Second)
			if status := verifyServiceHook(headers, body, []byte(fixtureSecret), staleNow, maxSkew, replayCache{}); status != http.StatusUnauthorized {
				t.Fatalf("stale timestamp: got status %d", status)
			}

			seen := replayCache{}
			if status := verifyServiceHook(headers, body, []byte(fixtureSecret), now, maxSkew, seen); status != http.StatusNoContent {
				t.Fatalf("first delivery: got status %d", status)
			}
			if status := verifyServiceHook(headers, body, []byte(fixtureSecret), now, maxSkew, seen); status != http.StatusConflict {
				t.Fatalf("replay: got status %d", status)
			}
		})
	}
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return data
}
