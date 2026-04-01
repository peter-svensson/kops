/*
Copyright 2022 The Kubernetes Authors.

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

package scalewaymodel

import (
	"fmt"
	"strings"

	"github.com/scaleway/scaleway-sdk-go/scw"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/model"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/scaleway"
	"k8s.io/kops/upup/pkg/fi/cloudup/scalewaytasks"
)

var commercialTypesWithBlockStorageOnly = []string{"PRO", "PLAY", "ENT"}

const (
	defaultNodeRootVolumeSizeGB         = 50
	defaultControlPlaneRootVolumeSizeGB = 20
)

// InstanceModelBuilder configures scaling groups for the cluster
type InstanceModelBuilder struct {
	*ScwModelContext

	BootstrapScriptBuilder *model.BootstrapScriptBuilder
	Lifecycle              fi.Lifecycle
}

var _ fi.CloudupModelBuilder = &InstanceModelBuilder{}

func (b *InstanceModelBuilder) Build(c *fi.CloudupModelBuilderContext) error {
	for _, ig := range b.InstanceGroups {
		name := ig.Name
		zone, err := scw.ParseZone(ig.Spec.Subnets[0])
		if err != nil {
			return fmt.Errorf("error building scaling group for %q: %w", name, err)
		}

		subnets, err := b.GatherSubnets(ig)
		if err != nil {
			return fmt.Errorf("error gathering subnets for %q: %w", name, err)
		}

		userData, err := b.BootstrapScriptBuilder.ResourceNodeUp(c, ig)
		if err != nil {
			return fmt.Errorf("error building bootstrap script for %q: %w", name, err)
		}

		tags := []string{
			scaleway.TagClusterName + "=" + b.Cluster.Name,
			scaleway.TagInstanceGroup + "=" + ig.Name,
		}
		for k, v := range b.CloudTags(b.ClusterName(), false) {
			tags = append(tags, fmt.Sprintf("%s=%s", k, v))
		}
		if ig.IsControlPlane() {
			tags = append(tags, scaleway.TagNameRolePrefix+"="+scaleway.TagRoleControlPlane)
		}

		// Private network attachment
		var privateNetworkIDs []string
		if len(subnets) > 0 && subnets[0].Type == kops.SubnetTypePrivate {
			if subnets[0].ID == "" {
				return fmt.Errorf("private subnet %q for instance group %q must have an ID (Scaleway Private Network UUID)", subnets[0].Name, name)
			}
			privateNetworkIDs = []string{subnets[0].ID}
		}

		// Root volume size for block-storage-only instance types
		var rootVolumeSize *int
		for _, commercialType := range commercialTypesWithBlockStorageOnly {
			if strings.HasPrefix(ig.Spec.MachineType, commercialType) {
				if ig.IsControlPlane() {
					rootVolumeSize = fi.PtrTo(defaultControlPlaneRootVolumeSizeGB)
				} else {
					rootVolumeSize = fi.PtrTo(defaultNodeRootVolumeSizeGB)
				}
				break
			}
		}

		// Create instance template (defines what instances look like)
		template := &scalewaytasks.InstanceTemplate{
			Name:              fi.PtrTo(name),
			Lifecycle:         b.Lifecycle,
			Zone:              fi.PtrTo(string(zone)),
			CommercialType:    fi.PtrTo(ig.Spec.MachineType),
			ImageID:           fi.PtrTo(ig.Spec.Image),
			UserData:          &userData,
			Tags:              tags,
			PrivateNetworkIDs: privateNetworkIDs,
			RootVolumeSize:    rootVolumeSize,
		}
		c.AddTask(template)

		// Determine min/max replicas
		// The Scaleway autoscaling API requires max_replicas >= 2
		minReplicas := uint32(fi.ValueOf(ig.Spec.MinSize))
		maxReplicas := uint32(fi.ValueOf(ig.Spec.MaxSize))
		if maxReplicas < 2 {
			maxReplicas = 2
		}

		// Create scaling group (defines how many and LB integration)
		group := &scalewaytasks.InstanceScalingGroup{
			Name:             fi.PtrTo(name),
			Lifecycle:        b.Lifecycle,
			Zone:             fi.PtrTo(string(zone)),
			MinReplicas:      fi.PtrTo(minReplicas),
			MaxReplicas:      fi.PtrTo(maxReplicas),
			Tags:             tags,
			InstanceTemplate: template,
		}

		// The Scaleway autoscaling v1alpha1 API requires a loadbalancer
		// for all instance groups. Attach the API LB to all groups.
		if b.UseLoadBalancerForAPI() {
			group.LoadBalancer = b.LinkToScalewayLoadBalancer()
			group.LBBackends = b.LBBackends
			if len(privateNetworkIDs) > 0 {
				group.LBPrivateNetworkID = fi.PtrTo(privateNetworkIDs[0])
			}
		}

		c.AddTask(group)
	}
	return nil
}
