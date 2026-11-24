package k8s

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// ParseHelperResources parses the resources of the helper containers of task
// pods (copy-tools, stage-in) from JSON in the shape of a Kubernetes
// container's `resources`, e.g.
//
//	{"requests": {"cpu": "100m", "memory": "128Mi"}, "limits": {"cpu": "500m", "memory": "256Mi"}}
//
// An empty string means no resources: the containers get none, as before the
// setting existed, and the namespace's LimitRange (if any) applies. Set them
// where a ResourceQuota requires requests or limits on every container.
func ParseHelperResources(raw string) (corev1.ResourceRequirements, error) {
	var res corev1.ResourceRequirements
	if strings.TrimSpace(raw) == "" {
		return res, nil
	}

	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil {
		return corev1.ResourceRequirements{}, fmt.Errorf("parse helper container resources: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return corev1.ResourceRequirements{}, fmt.Errorf("parse helper container resources: trailing data after the JSON object")
	}

	for _, list := range []corev1.ResourceList{res.Requests, res.Limits} {
		for name, q := range list {
			if q.Sign() < 0 {
				return corev1.ResourceRequirements{}, fmt.Errorf("helper container resources: %s must not be negative, got %s", name, q.String())
			}
		}
	}
	for _, name := range sortedResourceNames(res.Requests) {
		request := res.Requests[name]
		limit, ok := res.Limits[name]
		if ok && request.Cmp(limit) > 0 {
			return corev1.ResourceRequirements{}, fmt.Errorf("helper container resources: %s request %s exceeds its limit %s", name, request.String(), limit.String())
		}
	}
	return res, nil
}

// helperResources returns a copy of the configured helper container
// resources for one container, so that containers never share the maps.
func helperResources(cfg Config) corev1.ResourceRequirements {
	return *cfg.HelperResources.DeepCopy()
}

func sortedResourceNames(list corev1.ResourceList) []corev1.ResourceName {
	names := make([]corev1.ResourceName, 0, len(list))
	for name := range list {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		return names[i] < names[j]
	})
	return names
}
