/*
Copyright 2026 Sentry Kubernetes Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package telegramreceiver implements the public Telegram webhook edge. It has
// no Kubernetes client and forwards only a one-time code plus numeric chat/topic
// identity to the protected registration writer.
package telegramreceiver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
)

const (
	WebhookPath          = "/v1/telegram/webhook"
	TelegramSecretHeader = "X-Telegram-Bot-Api-Secret-Token"
	webhookBodyLimit     = 64 << 10
)

var (
	ErrWriterUnavailable = errors.New("registration writer unavailable")
	ErrWriterPending     = errors.New("registration publish pending")
	ErrWriterRejected    = errors.New("registration rejected")
)

type SecretProvider interface {
	Secret(context.Context) (string, error)
}

type Writer interface {
	Redeem(context.Context, string, binding.TelegramTarget, string) error
}

type Handler struct {
	Secret      SecretProvider
	Writer      Writer
	BotUsername string
}

type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message"`
}

type message struct {
	Text            string          `json:"text"`
	Entities        []messageEntity `json:"entities"`
	MessageThreadID *int64          `json:"message_thread_id"`
	Chat            chat            `json:"chat"`
}

type chat struct {
	ID int64 `json:"id"`
}

type messageEntity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

func (h *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Pragma", "no-cache")
	if request.Method != http.MethodPost || request.URL.Path != WebhookPath {
		http.Error(response, "not found", http.StatusNotFound)
		return
	}
	if h.Secret == nil || h.Writer == nil {
		http.Error(response, "receiver unavailable", http.StatusServiceUnavailable)
		return
	}
	secret, err := h.Secret.Secret(request.Context())
	if err != nil || !validWebhookSecret(secret) || !constantTimeEqual(secret, request.Header.Get(TelegramSecretHeader)) {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(response, "invalid request", http.StatusBadRequest)
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, webhookBodyLimit)
	input, err := decodeUpdate(request.Body)
	if err != nil {
		// A syntactically invalid update can never become valid on retry.
		response.WriteHeader(http.StatusNoContent)
		return
	}
	code, target, ok := registrationCommand(input, h.BotUsername)
	if !ok {
		response.WriteHeader(http.StatusNoContent)
		return
	}
	operationKey := updateOperationKey(input.UpdateID, code)
	if err := h.Writer.Redeem(request.Context(), code, target, operationKey); errors.Is(err, ErrWriterRejected) {
		// The code is expired, already consumed, or no longer authorized. Retrying
		// the same Telegram update cannot repair it, so acknowledge and drop it.
		response.WriteHeader(http.StatusNoContent)
		return
	} else if err != nil {
		http.Error(response, "registration unavailable", http.StatusServiceUnavailable)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func decodeUpdate(body io.Reader) (update, error) {
	decoder := json.NewDecoder(body)
	var input update
	if err := decoder.Decode(&input); err != nil {
		return update{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return update{}, errors.New("one JSON object required")
	}
	return input, nil
}

func registrationCommand(input update, botUsername string) (string, binding.TelegramTarget, bool) {
	if input.UpdateID < 0 || input.Message == nil || input.Message.Chat.ID == 0 || input.Message.Text == "" {
		return "", binding.TelegramTarget{}, false
	}
	var commandEntity *messageEntity
	for i := range input.Message.Entities {
		entity := &input.Message.Entities[i]
		if entity.Type == "bot_command" && entity.Offset == 0 {
			commandEntity = entity
			break
		}
	}
	if commandEntity == nil {
		return "", binding.TelegramTarget{}, false
	}
	command, rest, ok := splitUTF16Entity(input.Message.Text, commandEntity.Length)
	if !ok || !acceptedCommand(command, botUsername) {
		return "", binding.TelegramTarget{}, false
	}
	fields := strings.Fields(rest)
	if len(fields) != 1 {
		return "", binding.TelegramTarget{}, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(fields[0])
	if err != nil || len(decoded) != 32 {
		return "", binding.TelegramTarget{}, false
	}
	target := binding.TelegramTarget{ChatIdentity: strconv.FormatInt(input.Message.Chat.ID, 10)}
	if input.Message.MessageThreadID != nil {
		if *input.Message.MessageThreadID <= 0 {
			return "", binding.TelegramTarget{}, false
		}
		target.TopicIdentity = strconv.FormatInt(*input.Message.MessageThreadID, 10)
	}
	return fields[0], target, true
}

func splitUTF16Entity(text string, length int) (string, string, bool) {
	units := utf16.Encode([]rune(text))
	if length <= 0 || length > len(units) {
		return "", "", false
	}
	command := string(utf16.Decode(units[:length]))
	rest := string(utf16.Decode(units[length:]))
	return command, rest, true
}

func acceptedCommand(command, botUsername string) bool {
	if command == "/register" {
		return true
	}
	if botUsername == "" {
		return false
	}
	return strings.EqualFold(command, "/register@"+strings.TrimPrefix(botUsername, "@"))
}

func updateOperationKey(updateID int64, code string) string {
	digest := sha256.Sum256([]byte("telegram-update-v1\x00" + strconv.FormatInt(updateID, 10) + "\x00" + code))
	return hex.EncodeToString(digest[:])
}

func validWebhookSecret(secret string) bool {
	if len(secret) < 1 || len(secret) > 256 {
		return false
	}
	for _, character := range secret {
		if (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func constantTimeEqual(expected, actual string) bool {
	if len(expected) != len(actual) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}
