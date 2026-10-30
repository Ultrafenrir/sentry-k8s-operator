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

package registration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/destination"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/policy"
)

const (
	ChallengePath       = "/v1/telegram/challenges"
	challengeBodyLimit  = 4096
	defaultChallengeTTL = 10 * time.Minute
)

type TokenReviewer interface {
	Create(context.Context, *authenticationv1.TokenReview, metav1.CreateOptions) (*authenticationv1.TokenReview, error)
}

type SubjectAccessReviewer interface {
	Create(context.Context, *authorizationv1.SubjectAccessReview, metav1.CreateOptions) (*authorizationv1.SubjectAccessReview, error)
}

// ChallengeHandler is the Kubernetes-authenticated registration endpoint. It performs
// TokenReview and exact-name SubjectAccessReview before reading project or protected state.
type ChallengeHandler struct {
	Tokens       TokenReviewer
	Access       SubjectAccessReviewer
	Client       client.Client
	Registration *Service
	Audiences    []string
	ChallengeTTL time.Duration
	Now          func() time.Time
}

type challengeRequest struct {
	Namespace   string `json:"namespace"`
	Project     string `json:"project"`
	Destination string `json:"destination"`
}

type challengeResponse struct {
	Command   string      `json:"command"`
	ExpiresAt metav1.Time `json:"expiresAt"`
}

func (h *ChallengeHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Pragma", "no-cache")
	if request.URL.Path != ChallengePath {
		writeChallengeError(response, http.StatusNotFound, "not found")
		return
	}
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		writeChallengeError(response, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.Tokens == nil || h.Access == nil || h.Client == nil || h.Registration == nil || h.Registration.Store == nil || h.Registration.Index == nil || len(h.Audiences) == 0 {
		writeChallengeError(response, http.StatusServiceUnavailable, "registration service unavailable")
		return
	}
	token, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok {
		response.Header().Set("WWW-Authenticate", "Bearer")
		writeChallengeError(response, http.StatusUnauthorized, "authentication required")
		return
	}
	user, ok := h.authenticate(request.Context(), token)
	if !ok {
		response.Header().Set("WWW-Authenticate", "Bearer")
		writeChallengeError(response, http.StatusUnauthorized, "authentication failed")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, challengeBodyLimit)
	input, err := decodeChallengeRequest(request)
	if err != nil {
		writeChallengeError(response, http.StatusBadRequest, "invalid request")
		return
	}
	allowed, reviewed := h.authorize(request.Context(), user, input)
	if !reviewed {
		writeChallengeError(response, http.StatusServiceUnavailable, "authorization service unavailable")
		return
	}
	if !allowed {
		writeChallengeError(response, http.StatusForbidden, "access denied")
		return
	}
	code, err := h.issue(request.Context(), input)
	if err != nil {
		writeChallengeError(response, http.StatusConflict, "registration challenge unavailable")
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(response).Encode(challengeResponse{
		Command:   "/register " + code,
		ExpiresAt: metav1.NewTime(h.now().Add(h.ttl())),
	})
}

func (h *ChallengeHandler) authenticate(ctx context.Context, token string) (authenticationv1.UserInfo, bool) {
	review, err := h.Tokens.Create(ctx, &authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{
		Token: token, Audiences: append([]string(nil), h.Audiences...),
	}}, metav1.CreateOptions{})
	if err != nil || review == nil || !review.Status.Authenticated || review.Status.Error != "" || review.Status.User.Username == "" || !audiencesIntersect(h.Audiences, review.Status.Audiences) {
		return authenticationv1.UserInfo{}, false
	}
	return review.Status.User, true
}

func (h *ChallengeHandler) authorize(ctx context.Context, user authenticationv1.UserInfo, input challengeRequest) (bool, bool) {
	extra := make(map[string]authorizationv1.ExtraValue, len(user.Extra))
	for key, values := range user.Extra {
		extra[key] = authorizationv1.ExtraValue(append([]string(nil), values...))
	}
	review, err := h.Access.Create(ctx, &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User: user.Username, UID: user.UID, Groups: append([]string(nil), user.Groups...), Extra: extra,
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: input.Namespace, Verb: "update", Group: sentryv1alpha1.GroupVersion.Group,
			Version: sentryv1alpha1.GroupVersion.Version, Resource: "sentryprojects", Name: input.Project,
		},
	}}, metav1.CreateOptions{})
	if err != nil || review == nil || review.Status.EvaluationError != "" {
		return false, false
	}
	return review.Status.Allowed && !review.Status.Denied, true
}

func (h *ChallengeHandler) issue(ctx context.Context, input challengeRequest) (string, error) {
	project := &sentryv1alpha1.SentryProject{}
	if err := h.Client.Get(ctx, types.NamespacedName{Namespace: input.Namespace, Name: input.Project}, project); err != nil || project.DeletionTimestamp != nil || project.Spec.Paused {
		return "", ErrRegistrationRequired
	}
	record, err := h.Registration.Store.GetBinding(ctx, string(project.UID))
	if err != nil || record.Binding.Project != (binding.ProjectReference{Namespace: project.Namespace, Name: project.Name, UID: string(project.UID), Generation: project.Generation}) || record.Binding.AccessMode != binding.AccessManage || record.Binding.Owner == "" {
		return "", ErrRegistrationRequired
	}
	connection := &sentryv1alpha1.SentryConnection{}
	if err := h.Client.Get(ctx, types.NamespacedName{Name: record.Binding.Connection}, connection); err != nil || connection.DeletionTimestamp != nil {
		return "", ErrRegistrationRequired
	}
	if project.Spec.Connection != record.Binding.Connection && !(project.Spec.Connection == "" && connection.Spec.Default) {
		return "", ErrRegistrationRequired
	}
	desired, found := project.Spec.Destinations[input.Destination]
	protected, protectedFound := record.Binding.Destinations[input.Destination]
	decision, err := policy.EvaluateDestination(project.Namespace, project.Spec, connection.Spec, desired)
	if !found || err != nil || decision.AccessMode != binding.AccessManage || decision.Owner != record.Binding.Owner {
		return "", ErrRegistrationRequired
	}
	kind, selectorHash, err := destination.SelectorFingerprint(connection.Spec, desired)
	if !protectedFound || err != nil || kind != "telegram" || protected.Kind != kind || protected.SelectorHash != selectorHash || protected.State != destination.StateRegistrationRequired {
		return "", ErrRegistrationRequired
	}
	return h.Registration.Issue(ctx, IssueInput{
		Project: record.Binding.Project, Destination: input.Destination, Owner: record.Binding.Owner,
		Connection: record.Binding.Connection, PolicyGeneration: connection.Generation,
		PolicyResourceVersion: connection.ResourceVersion, TTL: h.ttl(),
	})
}

func decodeChallengeRequest(request *http.Request) (challengeRequest, error) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return challengeRequest{}, errors.New("content type must be application/json")
	}
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var input challengeRequest
	if err := decoder.Decode(&input); err != nil {
		return challengeRequest{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return challengeRequest{}, errors.New("request must contain one JSON object")
	}
	if len(input.Namespace) > 63 || len(input.Project) > 253 || len(input.Destination) > 63 || len(utilvalidation.IsDNS1123Label(input.Namespace)) != 0 || len(utilvalidation.IsDNS1123Subdomain(input.Project)) != 0 || len(utilvalidation.IsDNS1123Label(input.Destination)) != 0 {
		return challengeRequest{}, errors.New("invalid object reference")
	}
	return input, nil
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	returnValue := ""
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		returnValue = parts[1]
	}
	return returnValue, returnValue != "" && len(returnValue) <= 16384
}

func audiencesIntersect(expected, actual []string) bool {
	for _, left := range expected {
		for _, right := range actual {
			if left == right {
				return true
			}
		}
	}
	return false
}

func writeChallengeError(response http.ResponseWriter, status int, message string) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(map[string]string{"error": message})
}

func (h *ChallengeHandler) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

func (h *ChallengeHandler) ttl() time.Duration {
	if h.ChallengeTTL > 0 {
		return h.ChallengeTTL
	}
	return defaultChallengeTTL
}
