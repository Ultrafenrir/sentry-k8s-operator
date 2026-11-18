/*
Copyright 2026 Sentry Kubernetes Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package alerts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// ModernGraphPlan is immutable desired topology. Equal workflow hashes are one reusable
// workflow template; each detector key remains independently owned and linked.
type ModernGraphPlan struct {
	Detectors []ModernDetectorTemplate
	Workflows []ModernWorkflowTemplate
	Links     []ModernTemplateLink
}

type ModernDetectorTemplate struct {
	Key  string
	Hash string
}
type ModernWorkflowTemplate struct {
	Hash         string
	ConsumerKeys []string
}
type ModernTemplateLink struct {
	DetectorKey  string
	WorkflowHash string
}

func PlanModernGraph(desired map[string]Desired) ModernGraphPlan {
	keys := make([]string, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	plan := ModernGraphPlan{}
	workflows := map[string][]string{}
	for _, key := range keys {
		item := normalize(desired[key])
		detectorHash := desiredHash(item)
		workflowHash := modernWorkflowHash(item)
		plan.Detectors = append(plan.Detectors, ModernDetectorTemplate{Key: key, Hash: detectorHash})
		plan.Links = append(plan.Links, ModernTemplateLink{DetectorKey: key, WorkflowHash: workflowHash})
		workflows[workflowHash] = append(workflows[workflowHash], key)
	}
	hashes := make([]string, 0, len(workflows))
	for hash := range workflows {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	for _, hash := range hashes {
		plan.Workflows = append(plan.Workflows, ModernWorkflowTemplate{Hash: hash, ConsumerKeys: append([]string(nil), workflows[hash]...)})
	}
	return plan
}

func modernWorkflowHash(desired Desired) string {
	canonical := normalize(desired)
	value := struct {
		Issue    *IssueDesired   `json:"issue,omitempty"`
		Notify   []string        `json:"notify,omitempty"`
		Actions  []ActionDesired `json:"actions,omitempty"`
		Throttle int32           `json:"throttle"`
	}{Issue: canonical.Issue, Notify: canonical.Notify, Actions: canonical.Actions, Throttle: canonical.ThrottleMinutes}
	payload, _ := json.Marshal(value)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
