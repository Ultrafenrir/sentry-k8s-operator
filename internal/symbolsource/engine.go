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

package symbolsource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
)

const (
	StateReady   = "Ready"
	StateDeleted = "Deleted"
	StateError   = "Error"
)

var (
	ErrInvalidInput    = errors.New("custom symbol-source desired state is invalid")
	ErrOwnershipLost   = errors.New("custom symbol-source ownership evidence is missing")
	ErrExternalMissing = errors.New("custom symbol source is missing")
	ErrVerification    = errors.New("custom symbol-source mutation verification failed")
	localNamePattern   = regexp.MustCompile(`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`)
)

type Desired struct {
	Type        string
	Layout      Layout
	URL         string
	Username    string
	Password    string
	Bucket      string
	Region      string
	AccessKey   string
	SecretKey   string
	Prefix      string
	ClientEmail string
	PrivateKey  string
}

type Input struct {
	Organization string
	ProjectSlug  string
	ProjectUID   string
	Desired      map[string]Desired
	Current      map[string]binding.SymbolSourceBinding
	Authorize    func(context.Context) error
}

type Status struct {
	State  string
	Reason string
	Err    error
}

type Result struct {
	Bindings map[string]binding.SymbolSourceBinding
	Statuses map[string]Status
}

func (r Result) Err() error {
	errs := make([]error, 0, len(r.Statuses))
	for _, status := range r.Statuses {
		if status.Err != nil {
			errs = append(errs, status.Err)
		}
	}
	return errors.Join(errs...)
}

type Engine struct {
	Adapter Adapter
}

func (e *Engine) Reconcile(ctx context.Context, input Input) Result {
	result := Result{Bindings: cloneBindings(input.Current), Statuses: make(map[string]Status)}
	if e == nil || e.Adapter == nil || input.Authorize == nil || input.Organization == "" || input.ProjectSlug == "" || input.ProjectUID == "" || len(input.Desired) > 20 {
		result.Statuses[""] = errorStatus(ErrInvalidInput)
		return result
	}
	for name, desired := range input.Desired {
		if !validDesired(name, desired) {
			result.Statuses[name] = errorStatus(ErrInvalidInput)
			return result
		}
	}
	for _, name := range removedNames(input.Current, input.Desired) {
		if err := e.deleteOne(ctx, input, name, input.Current[name]); err != nil {
			result.Statuses[name] = errorStatus(err)
			continue
		}
		delete(result.Bindings, name)
		result.Statuses[name] = Status{State: StateDeleted, Reason: "SymbolSourceDeleted"}
	}
	for _, name := range desiredNames(input.Desired) {
		current, found := input.Current[name]
		next, err := e.reconcileOne(ctx, input, name, input.Desired[name], current, found)
		if err != nil {
			result.Statuses[name] = errorStatus(err)
			continue
		}
		result.Bindings[name] = next
		result.Statuses[name] = Status{State: StateReady, Reason: "SymbolSourceReady"}
	}
	return result
}

func (e *Engine) reconcileOne(ctx context.Context, input Input, name string, desired Desired, current binding.SymbolSourceBinding, found bool) (binding.SymbolSourceBinding, error) {
	identity := remoteIdentity(input.ProjectUID, name)
	remoteName := remoteName(input.ProjectUID, name)
	hash := desiredHash(desired)
	if found && (current.ExternalIdentity != identity || current.RemoteName != remoteName || current.Type != desired.Type) {
		return binding.SymbolSourceBinding{}, ErrOwnershipLost
	}
	remote, remoteFound, err := e.observe(ctx, input, identity)
	if err != nil {
		return binding.SymbolSourceBinding{}, err
	}
	if found && !remoteFound {
		return binding.SymbolSourceBinding{}, ErrExternalMissing
	}
	if remoteFound && (remote.Name != remoteName || remote.Type != desired.Type) {
		return binding.SymbolSourceBinding{}, ErrOwnershipLost
	}
	request := desiredRequest(identity, remoteName, desired)
	if !remoteFound {
		if err := input.Authorize(ctx); err != nil {
			return binding.SymbolSourceBinding{}, err
		}
		remote, err = e.Adapter.Create(ctx, input.Organization, input.ProjectSlug, request)
		if err != nil {
			remote, remoteFound, _ = e.observe(ctx, input, identity)
			if !remoteFound {
				return binding.SymbolSourceBinding{}, err
			}
			// A recovered deterministic identity proves which child was created, but the
			// redacted GET cannot prove its credential value. Repeat the complete PUT and
			// require an acknowledged response before committing its desired hash.
			remote, err = e.Adapter.Update(ctx, input.Organization, input.ProjectSlug, identity, request)
			if err != nil {
				return binding.SymbolSourceBinding{}, err
			}
		}
	} else if !found || current.DesiredHash != hash || !visibleEqual(remote, request) {
		if err := input.Authorize(ctx); err != nil {
			return binding.SymbolSourceBinding{}, err
		}
		remote, err = e.Adapter.Update(ctx, input.Organization, input.ProjectSlug, identity, request)
		if err != nil {
			return binding.SymbolSourceBinding{}, err
		}
	}
	verified, verifiedFound, err := e.observe(ctx, input, identity)
	if err != nil {
		return binding.SymbolSourceBinding{}, err
	}
	if !verifiedFound || !visibleEqual(verified, request) || verified.Name != remoteName || verified.Type != desired.Type {
		return binding.SymbolSourceBinding{}, ErrVerification
	}
	return binding.SymbolSourceBinding{ExternalIdentity: identity, RemoteName: remoteName, Type: desired.Type, DesiredHash: hash}, nil
}

func (e *Engine) deleteOne(ctx context.Context, input Input, name string, current binding.SymbolSourceBinding) error {
	identity := remoteIdentity(input.ProjectUID, name)
	if current.ExternalIdentity != identity || current.RemoteName != remoteName(input.ProjectUID, name) || (current.Type != "http" && current.Type != "s3" && current.Type != "gcs") {
		return ErrOwnershipLost
	}
	remote, found, err := e.observe(ctx, input, identity)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if remote.Name != current.RemoteName || remote.Type != current.Type {
		return ErrOwnershipLost
	}
	if err := input.Authorize(ctx); err != nil {
		return err
	}
	if err := e.Adapter.Delete(ctx, input.Organization, input.ProjectSlug, identity); err != nil {
		if _, stillFound, _ := e.observe(ctx, input, identity); stillFound {
			return err
		}
	}
	if _, stillFound, err := e.observe(ctx, input, identity); err != nil {
		return err
	} else if stillFound {
		return ErrVerification
	}
	return nil
}

func (e *Engine) observe(ctx context.Context, input Input, identity string) (Remote, bool, error) {
	items, err := e.Adapter.List(ctx, input.Organization, input.ProjectSlug)
	if err != nil {
		return Remote{}, false, err
	}
	for _, item := range items {
		if item.Identity == identity {
			return item, true, nil
		}
	}
	return Remote{}, false, nil
}

func desiredRequest(identity, name string, desired Desired) Request {
	return Request{
		Identity: identity, Name: name, Type: desired.Type, Layout: desired.Layout,
		URL: desired.URL, Username: desired.Username, Password: desired.Password,
		Bucket: desired.Bucket, Region: desired.Region, AccessKey: desired.AccessKey,
		SecretKey: desired.SecretKey, Prefix: desired.Prefix, ClientEmail: desired.ClientEmail,
		PrivateKey: desired.PrivateKey,
	}
}

func visibleEqual(remote Remote, request Request) bool {
	return remote.Identity == request.Identity && remote.Name == request.Name && remote.Type == request.Type &&
		remote.Layout == request.Layout && remote.URL == request.URL && remote.Username == request.Username &&
		remote.Bucket == request.Bucket && remote.Region == request.Region && remote.AccessKey == request.AccessKey &&
		remote.Prefix == request.Prefix && remote.ClientEmail == request.ClientEmail
}

func validDesired(name string, desired Desired) bool {
	if len(name) > 32 || !localNamePattern.MatchString(name) || !validLayout(desired.Layout) {
		return false
	}
	switch desired.Type {
	case "http":
		parsed, err := url.ParseRequestURI(desired.URL)
		return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && desired.Bucket == "" && desired.Region == "" && desired.AccessKey == "" && desired.SecretKey == "" && desired.ClientEmail == "" && desired.PrivateKey == ""
	case "s3":
		return desired.Bucket != "" && validRegion(desired.Region) && desired.AccessKey != "" && desired.SecretKey != "" && desired.URL == "" && desired.Username == "" && desired.Password == "" && desired.ClientEmail == "" && desired.PrivateKey == ""
	case "gcs":
		return desired.Bucket != "" && strings.Contains(desired.ClientEmail, "@") && desired.PrivateKey != "" && desired.URL == "" && desired.Username == "" && desired.Password == "" && desired.Region == "" && desired.AccessKey == "" && desired.SecretKey == ""
	default:
		return false
	}
}

func validLayout(layout Layout) bool {
	types := map[string]bool{"native": true, "symstore": true, "symstore_index2": true, "ssqp": true, "unified": true, "debuginfod": true}
	casings := map[string]bool{"default": true, "uppercase": true, "lowercase": true}
	return types[layout.Type] && casings[layout.Casing]
}

func validRegion(region string) bool {
	regions := map[string]bool{
		"us-east-2": true, "us-east-1": true, "us-west-1": true, "us-west-2": true,
		"ap-east-1": true, "ap-south-1": true, "ap-northeast-2": true, "ap-southeast-1": true,
		"ap-southeast-2": true, "ap-northeast-1": true, "ca-central-1": true, "cn-north-1": true,
		"cn-northwest-1": true, "eu-central-1": true, "eu-west-1": true, "eu-west-2": true,
		"eu-west-3": true, "eu-north-1": true, "sa-east-1": true, "us-gov-east-1": true, "us-gov-west-1": true,
	}
	return regions[region]
}

func desiredHash(desired Desired) string {
	payload, _ := json.Marshal(desired)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func remoteIdentity(projectUID, name string) string {
	digest := sha256.Sum256([]byte(projectUID + "\x00" + name))
	return "sko-" + hex.EncodeToString(digest[:16])
}

func remoteName(projectUID, name string) string {
	digest := sha256.Sum256([]byte(projectUID))
	return "sko:" + name + ":" + hex.EncodeToString(digest[:6])
}

func removedNames(current map[string]binding.SymbolSourceBinding, desired map[string]Desired) []string {
	result := make([]string, 0)
	for name := range current {
		if _, found := desired[name]; !found {
			result = append(result, name)
		}
	}
	sort.Strings(result)
	return result
}

func desiredNames(desired map[string]Desired) []string {
	result := make([]string, 0, len(desired))
	for name := range desired {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func cloneBindings(source map[string]binding.SymbolSourceBinding) map[string]binding.SymbolSourceBinding {
	result := make(map[string]binding.SymbolSourceBinding, len(source))
	for name, value := range source {
		result[name] = value
	}
	return result
}

func errorStatus(err error) Status {
	reason := "SymbolSourceOperationFailed"
	switch {
	case errors.Is(err, ErrInvalidInput):
		reason = "InvalidSymbolSource"
	case errors.Is(err, ErrOwnershipLost):
		reason = "SymbolSourceOwnershipLost"
	case errors.Is(err, ErrExternalMissing):
		reason = "SymbolSourceMissing"
	case errors.Is(err, ErrVerification):
		reason = "SymbolSourceVerificationFailed"
	}
	return Status{State: StateError, Reason: reason, Err: err}
}
