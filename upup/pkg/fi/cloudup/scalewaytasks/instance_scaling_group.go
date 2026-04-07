/*
Copyright 2025 The Kubernetes Authors.

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

package scalewaytasks

import (
	"fmt"
	"sort"
	"strings"

	autoscaling "github.com/scaleway/scaleway-sdk-go/api/autoscaling/v1alpha1"
	"github.com/scaleway/scaleway-sdk-go/scw"
	"k8s.io/klog/v2"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/scaleway"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraform"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraformWriter"
)

// +kops:fitask
type InstanceScalingGroup struct {
	Name      *string
	Lifecycle fi.Lifecycle

	Zone        *string
	GroupID     *string
	MinReplicas *uint32
	MaxReplicas *uint32
	Tags        []string

	InstanceTemplate   *InstanceTemplate
	LoadBalancer       *LoadBalancer
	LBBackends         []*LBBackend
	LBPrivateNetworkID *string
}

var (
	_ fi.CloudupTask            = (*InstanceScalingGroup)(nil)
	_ fi.CompareWithID          = (*InstanceScalingGroup)(nil)
	_ fi.CloudupHasDependencies = (*InstanceScalingGroup)(nil)
)

func (g *InstanceScalingGroup) CompareWithID() *string {
	return g.GroupID
}

func (g *InstanceScalingGroup) GetDependencies(tasks map[string]fi.CloudupTask) []fi.CloudupTask {
	var deps []fi.CloudupTask
	for _, task := range tasks {
		switch task.(type) {
		case *InstanceTemplate, *LoadBalancer, *LBBackend, *LBFrontend, *Volume:
			deps = append(deps, task)
		}
	}
	return deps
}

func (g *InstanceScalingGroup) Find(context *fi.CloudupContext) (*InstanceScalingGroup, error) {
	cloud := context.T.Cloud.(scaleway.ScwCloud)
	asService := cloud.AutoscalingService()

	groups, err := asService.ListInstanceGroups(&autoscaling.ListInstanceGroupsRequest{
		Zone: scw.Zone(cloud.Zone()),
	}, scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("listing instance groups: %w", err)
	}

	for _, grp := range groups.InstanceGroups {
		if grp.Name == fi.ValueOf(g.Name) {
			found := &InstanceScalingGroup{
				Name:      fi.PtrTo(grp.Name),
				GroupID:   fi.PtrTo(grp.ID),
				Zone:      g.Zone,
				Tags:      grp.Tags,
				Lifecycle: g.Lifecycle,
				InstanceTemplate: &InstanceTemplate{
					TemplateID: fi.PtrTo(grp.InstanceTemplateID),
				},
			}
			if grp.Capacity != nil {
				found.MinReplicas = fi.PtrTo(grp.Capacity.MinReplicas)
				found.MaxReplicas = fi.PtrTo(grp.Capacity.MaxReplicas)
			}
			if grp.Loadbalancer != nil {
				found.LoadBalancer = &LoadBalancer{LBID: fi.PtrTo(grp.Loadbalancer.ID)}
				for _, bid := range grp.Loadbalancer.BackendIDs {
					found.LBBackends = append(found.LBBackends, &LBBackend{ID: fi.PtrTo(bid)})
				}
				found.LBPrivateNetworkID = fi.PtrTo(grp.Loadbalancer.PrivateNetworkID)
			}
			return found, nil
		}
	}

	return nil, nil
}

func (g *InstanceScalingGroup) Run(context *fi.CloudupContext) error {
	return fi.CloudupDefaultDeltaRunMethod(g, context)
}

func (_ *InstanceScalingGroup) CheckChanges(actual, expected, changes *InstanceScalingGroup) error {
	if actual != nil {
		if changes.Name != nil {
			return fi.CannotChangeField("Name")
		}
		if changes.Zone != nil {
			return fi.CannotChangeField("Zone")
		}
	} else {
		if expected.Name == nil {
			return fi.RequiredField("Name")
		}
		if expected.Zone == nil {
			return fi.RequiredField("Zone")
		}
		if expected.InstanceTemplate == nil {
			return fi.RequiredField("InstanceTemplate")
		}
	}
	return nil
}

func (g *InstanceScalingGroup) RenderScw(target *scaleway.ScwAPITarget, actual, expected, changes *InstanceScalingGroup) error {
	asService := target.Cloud.AutoscalingService()
	zone := scw.Zone(fi.ValueOf(expected.Zone))

	var lbConfig *autoscaling.Loadbalancer
	if expected.LoadBalancer != nil {
		klog.Infof("InstanceScalingGroup %q: LB Name=%q LBID=%q", fi.ValueOf(expected.Name), fi.ValueOf(expected.LoadBalancer.Name), fi.ValueOf(expected.LoadBalancer.LBID))
	}
	if expected.LoadBalancer != nil && expected.LoadBalancer.LBID != nil {
		pnID := fi.ValueOf(expected.LBPrivateNetworkID)
		if parts := strings.SplitN(pnID, "/", 2); len(parts) == 2 {
			pnID = parts[1]
		}
		var backendIDs []string
		for _, backend := range expected.LBBackends {
			if backend.ID != nil {
				backendIDs = append(backendIDs, fi.ValueOf(backend.ID))
			}
		}
		lbConfig = &autoscaling.Loadbalancer{
			ID:               fi.ValueOf(expected.LoadBalancer.LBID),
			BackendIDs:       backendIDs,
			PrivateNetworkID: pnID,
		}
	}

	if actual != nil {
		// Only delete-and-recreate if template or LB backend IDs have
		// changed. Otherwise keep the existing group to avoid scale_down
		// events that block on the 5min cooldown.
		expectedTemplateID := fi.ValueOf(expected.InstanceTemplate.TemplateID)
		actualTemplateID := ""
		if actual.InstanceTemplate != nil {
			actualTemplateID = fi.ValueOf(actual.InstanceTemplate.TemplateID)
		}
		expectedBackendIDs := []string{}
		for _, b := range expected.LBBackends {
			if b.ID != nil {
				expectedBackendIDs = append(expectedBackendIDs, fi.ValueOf(b.ID))
			}
		}
		actualBackendIDs := []string{}
		for _, b := range actual.LBBackends {
			if b.ID != nil {
				actualBackendIDs = append(actualBackendIDs, fi.ValueOf(b.ID))
			}
		}
		sort.Strings(expectedBackendIDs)
		sort.Strings(actualBackendIDs)
		if actualTemplateID == expectedTemplateID && actualTemplateID != "" &&
			strings.Join(expectedBackendIDs, ",") == strings.Join(actualBackendIDs, ",") {
			klog.Infof("Instance scaling group %q unchanged, keeping existing group", fi.ValueOf(expected.Name))
			expected.GroupID = actual.GroupID
			return nil
		}
		klog.Infof("Deleting existing instance scaling group %q for recreation (template or backends changed)", fi.ValueOf(expected.Name))
		err := asService.DeleteInstanceGroup(&autoscaling.DeleteInstanceGroupRequest{
			Zone:            zone,
			InstanceGroupID: fi.ValueOf(actual.GroupID),
		})
		if err != nil {
			return fmt.Errorf("deleting instance scaling group %q: %w", fi.ValueOf(expected.Name), err)
		}
	}

	{
		klog.Infof("Creating instance scaling group %q", fi.ValueOf(expected.Name))

		projectID, err := target.Cloud.GetProjectID()
		if err != nil {
			return fmt.Errorf("getting project ID: %w", err)
		}

		req := &autoscaling.CreateInstanceGroupRequest{
			Zone:       zone,
			ProjectID:  projectID,
			Name:       fi.ValueOf(expected.Name),
			Tags:       expected.Tags,
			TemplateID: fi.ValueOf(expected.InstanceTemplate.TemplateID),
			Capacity: &autoscaling.Capacity{
				MinReplicas: fi.ValueOf(expected.MinReplicas),
				MaxReplicas: fi.ValueOf(expected.MaxReplicas),
				CooldownDelay: &scw.Duration{
					Seconds: 300, // 5 minutes
				},
			},
		}
		if lbConfig != nil {
			req.Loadbalancer = lbConfig
		}
		grp, err := asService.CreateInstanceGroup(req)
		if err != nil {
			return fmt.Errorf("creating instance scaling group: %w", err)
		}
		expected.GroupID = fi.PtrTo(grp.ID)
	}

	return nil
}

type terraformInstanceScalingGroup struct {
	Name               *string                  `cty:"name"`
	InstanceTemplateID *terraformWriter.Literal `cty:"instance_template_id"`
	MinReplicas        *uint32                  `cty:"min_replicas"`
	MaxReplicas        *uint32                  `cty:"max_replicas"`
	Tags               []string                 `cty:"tags"`
}

func (_ *InstanceScalingGroup) RenderTerraform(t *terraform.TerraformTarget, actual, expected, changes *InstanceScalingGroup) error {
	tfName := strings.ReplaceAll(fi.ValueOf(expected.Name), ".", "-")

	tf := terraformInstanceScalingGroup{
		Name:               expected.Name,
		InstanceTemplateID: expected.InstanceTemplate.TerraformLink(),
		MinReplicas:        expected.MinReplicas,
		MaxReplicas:        expected.MaxReplicas,
		Tags:               expected.Tags,
	}

	return t.RenderResource("scaleway_instance_scaling_group", tfName, tf)
}

func (g *InstanceScalingGroup) TerraformLink() *terraformWriter.Literal {
	tfName := strings.ReplaceAll(fi.ValueOf(g.Name), ".", "-")
	return terraformWriter.LiteralProperty("scaleway_instance_scaling_group", tfName, "id")
}
