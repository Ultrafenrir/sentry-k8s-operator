package alerts

import "testing"

func TestModernGraphPlannerReusesOnlyEquivalentWorkflowTemplates(t *testing.T) {
	base := Desired{Enabled: true, Notify: []string{"slack/incidents"}, ThrottleMinutes: 30, Metric: &MetricDesired{Preset: "error-count", IntervalMinutes: 5, Detection: DetectionDesired{Kind: "static", Direction: "above", High: 10}}}
	otherDetector := base
	otherDetector.Metric = &MetricDesired{Preset: "error-count", IntervalMinutes: 10, Detection: DetectionDesired{Kind: "static", Direction: "above", High: 25}}
	otherWorkflow := base
	otherWorkflow.Notify = []string{"pagerDuty/primary"}
	plan := PlanModernGraph(map[string]Desired{"second": otherDetector, "third": otherWorkflow, "first": base})
	if len(plan.Detectors) != 3 || len(plan.Workflows) != 2 || len(plan.Links) != 3 {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	if plan.Detectors[0].Key != "first" || plan.Detectors[1].Key != "second" || plan.Detectors[2].Key != "third" {
		t.Fatalf("detectors are not deterministic: %#v", plan.Detectors)
	}
	if plan.Links[0].WorkflowHash != plan.Links[1].WorkflowHash || plan.Links[0].WorkflowHash == plan.Links[2].WorkflowHash {
		t.Fatalf("workflow grouping is incorrect: %#v", plan.Links)
	}
}

func TestModernWorkflowHashIgnoresDetectorOnlyAndEnabledFields(t *testing.T) {
	first := Desired{Enabled: true, Description: "one", OwnerTeam: "backend", Notify: []string{"slack/incidents"}, Metric: &MetricDesired{Preset: "error-count", IntervalMinutes: 5, Detection: DetectionDesired{Kind: "static", Direction: "above", High: 10}}}
	second := first
	second.Enabled = false
	second.Description = "two"
	second.OwnerTeam = "platform"
	second.Metric = &MetricDesired{Preset: "affected-users", IntervalMinutes: 60, Detection: DetectionDesired{Kind: "static", Direction: "below", High: 2}}
	if modernWorkflowHash(first) != modernWorkflowHash(second) {
		t.Fatal("detector-only changes must not fork a workflow template")
	}
	second.ThrottleMinutes = 10
	if modernWorkflowHash(first) == modernWorkflowHash(second) {
		t.Fatal("workflow behavior changes must fork a workflow template")
	}
}
