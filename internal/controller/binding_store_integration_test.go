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
	"context"
	"errors"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
)

var _ = Describe("Protected binding store", func() {
	It("serializes concurrent manager claims through Kubernetes Create", func() {
		ctx := context.Background()
		store := &binding.Store{Client: k8sClient, Namespace: "default"}
		installationOne, err := binding.CanonicalInstallation("https://SENTRY.example/")
		Expect(err).NotTo(HaveOccurred())
		installationTwo, err := binding.CanonicalInstallation("https://sentry.example:443")
		Expect(err).NotTo(HaveOccurred())
		Expect(installationTwo).To(Equal(installationOne))

		claims := []binding.ManagerClaim{
			concurrentClaim(installationOne, "concurrent-one"),
			concurrentClaim(installationTwo, "concurrent-two"),
		}
		type outcome struct {
			record  binding.ClaimRecord
			created bool
			err     error
		}
		outcomes := make(chan outcome, len(claims))
		start := make(chan struct{})
		var wait sync.WaitGroup
		for _, claim := range claims {
			wait.Add(1)
			go func(claim binding.ManagerClaim) {
				defer GinkgoRecover()
				defer wait.Done()
				<-start
				record, created, acquireErr := store.AcquireClaim(ctx, claim)
				outcomes <- outcome{record: record, created: created, err: acquireErr}
			}(claim)
		}
		close(start)
		wait.Wait()
		close(outcomes)

		createdCount := 0
		conflictCount := 0
		var winner binding.ClaimRecord
		for result := range outcomes {
			switch {
			case result.err == nil && result.created:
				createdCount++
				winner = result.record
			case errors.Is(result.err, binding.ErrOwnershipConflict):
				conflictCount++
			default:
				Fail("unexpected claim outcome")
			}
		}
		Expect(createdCount).To(Equal(1))
		Expect(conflictCount).To(Equal(1))
		Expect(store.DeleteClaim(ctx, winner)).To(Succeed())
	})
})

func concurrentClaim(installation, uid string) binding.ManagerClaim {
	return binding.ManagerClaim{
		Version:      binding.CurrentVersion,
		Installation: installation,
		Organization: "company",
		Slug:         "concurrent-claim-test",
		Owner:        "payments",
		Manager: binding.ProjectReference{
			Namespace:  "default",
			Name:       uid,
			UID:        uid,
			Generation: 1,
		},
	}
}
