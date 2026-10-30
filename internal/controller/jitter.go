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

package controller

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

// jitterDuration spreads periodic and retry reconciles over ±10 percent. Failure
// to read randomness falls back to the bounded base duration, never to a tight loop.
func jitterDuration(base time.Duration) time.Duration {
	if base <= 0 {
		return base
	}
	var material [8]byte
	if _, err := rand.Read(material[:]); err != nil {
		return base
	}
	percent := int64(binary.LittleEndian.Uint64(material[:])%21) - 10
	return base + time.Duration(int64(base)*percent/100)
}
