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

package connection

import (
	"bytes"
	"testing"
)

func TestRegistryRotatesAllInputsWithoutRecreatingStableClient(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	first := SecretInputs{Token: []byte("token-one"), BotToken: []byte("bot-one")}
	clientOne, changed, err := registry.Apply("main", "https://sentry.example", first)
	if err != nil || !changed {
		t.Fatalf("initial Apply = %v, %v", changed, err)
	}
	versionOne, _ := registry.Versions("main")
	second := SecretInputs{Token: []byte("token-two"), BotToken: []byte("bot-two")}
	clientTwo, changed, err := registry.Apply("main", "https://sentry.example", second)
	if err != nil || !changed || clientOne != clientTwo {
		t.Fatalf("rotated Apply changed=%v sameClient=%v err=%v", changed, clientOne == clientTwo, err)
	}
	versionTwo, _ := registry.Versions("main")
	if versionOne == versionTwo {
		t.Fatal("rotation did not update safe input fingerprints")
	}
	loaded, _ := registry.Inputs("main")
	if !bytes.Equal(loaded.BotToken, second.BotToken) {
		t.Fatal("integration secrets were not rotated")
	}
	loaded.BotToken[0] = 'X'
	again, _ := registry.Inputs("main")
	if bytes.Equal(loaded.BotToken, again.BotToken) {
		t.Fatal("Inputs returned an aliased secret buffer")
	}
	_, changed, err = registry.Apply("main", "https://sentry.example", second)
	if err != nil || changed {
		t.Fatalf("unchanged Apply changed=%v err=%v", changed, err)
	}
	registry.Delete("main")
	if _, found := registry.Get("main"); found {
		t.Fatal("deleted registry entry remains available")
	}
}

func TestRegistryRejectsInvalidCARotationAtomically(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	clientBefore, _, err := registry.Apply("main", "https://sentry.example", SecretInputs{Token: []byte("one")})
	if err != nil {
		t.Fatal(err)
	}
	versionBefore, _ := registry.Versions("main")
	if _, _, err := registry.Apply("main", "https://sentry.example", SecretInputs{Token: []byte("two"), CABundle: []byte("invalid")}); err == nil {
		t.Fatal("invalid CA rotation succeeded")
	}
	clientAfter, _ := registry.Get("main")
	versionAfter, _ := registry.Versions("main")
	if clientBefore != clientAfter || versionBefore != versionAfter {
		t.Fatal("failed rotation changed the active registry entry")
	}
}

func TestRegistryRecreatesClientWhenTransportPolicyChanges(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	strict, _, err := registry.Apply("main", "https://sentry.example", SecretInputs{Token: []byte("one")})
	if err != nil {
		t.Fatal(err)
	}
	insecure, changed, err := registry.Apply("main", "https://sentry.example", SecretInputs{Token: []byte("one"), InsecureSkipTLSVerify: true})
	if err != nil || !changed || strict == insecure {
		t.Fatalf("transport update changed=%v recreated=%v err=%v", changed, strict != insecure, err)
	}
	loaded, _ := registry.Inputs("main")
	if !loaded.InsecureSkipTLSVerify {
		t.Fatal("transport policy was not retained")
	}
	if _, _, err := registry.Apply("http", "http://sentry.example", SecretInputs{Token: []byte("one")}); err == nil {
		t.Fatal("HTTP client was created without explicit opt-in")
	}
	if _, changed, err := registry.Apply("http", "http://sentry.example", SecretInputs{Token: []byte("one"), AllowInsecureHTTP: true}); err != nil || !changed {
		t.Fatalf("explicit HTTP client changed=%v err=%v", changed, err)
	}
}
