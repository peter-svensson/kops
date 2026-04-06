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
	"encoding/base64"
	"fmt"
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
type InstanceTemplate struct {
	Name      *string
	Lifecycle fi.Lifecycle

	Zone              *string
	TemplateID        *string
	CommercialType    *string
	ImageID           *string
	Tags              []string
	PrivateNetworkIDs []string
	UserData          *fi.Resource
	RootVolumeSize    *int // GB, for block-storage-only instance types
}

var _ fi.CompareWithID = (*InstanceTemplate)(nil)

func (t *InstanceTemplate) CompareWithID() *string {
	return t.TemplateID
}

func (t *InstanceTemplate) Find(context *fi.CloudupContext) (*InstanceTemplate, error) {
	cloud := context.T.Cloud.(scaleway.ScwCloud)
	asService := cloud.AutoscalingService()

	templates, err := asService.ListInstanceTemplates(&autoscaling.ListInstanceTemplatesRequest{
		Zone: scw.Zone(cloud.Zone()),
	}, scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("listing instance templates: %w", err)
	}

	for _, tmpl := range templates.InstanceTemplates {
		if tmpl.Name == fi.ValueOf(t.Name) {
			found := &InstanceTemplate{
				Name:              fi.PtrTo(tmpl.Name),
				TemplateID:        fi.PtrTo(tmpl.ID),
				Zone:              t.Zone,
				CommercialType:    fi.PtrTo(tmpl.CommercialType),
				ImageID:           tmpl.ImageID,
				Tags:              tmpl.Tags,
				PrivateNetworkIDs: tmpl.PrivateNetworkIDs,
				Lifecycle:         t.Lifecycle,
				UserData:          t.UserData,
			}
			return found, nil
		}
	}

	return nil, nil
}

func (t *InstanceTemplate) Run(context *fi.CloudupContext) error {
	return fi.CloudupDefaultDeltaRunMethod(t, context)
}

func (_ *InstanceTemplate) CheckChanges(actual, expected, changes *InstanceTemplate) error {
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
		if expected.CommercialType == nil {
			return fi.RequiredField("CommercialType")
		}
	}
	return nil
}

func (t *InstanceTemplate) RenderScw(target *scaleway.ScwAPITarget, actual, expected, changes *InstanceTemplate) error {
	asService := target.Cloud.AutoscalingService()
	zone := scw.Zone(fi.ValueOf(expected.Zone))

	var cloudInit *[]byte
	if expected.UserData != nil {
		userData, err := fi.ResourceAsBytes(*expected.UserData)
		if err != nil {
			return fmt.Errorf("reading user data: %w", err)
		}
		cloudInit = &userData
	}

	if actual != nil {
		// The v1alpha1 UpdateInstanceTemplate API returns 500. Delete and
		// recreate to ensure cloud-init and other config are current.
		// First delete any scaling groups that reference this template,
		// otherwise template deletion is rejected.
		groups, err := asService.ListInstanceGroups(&autoscaling.ListInstanceGroupsRequest{
			Zone: zone,
		}, scw.WithAllPages())
		if err != nil {
			return fmt.Errorf("listing scaling groups before template delete: %w", err)
		}
		for _, grp := range groups.InstanceGroups {
			if grp.InstanceTemplateID == fi.ValueOf(actual.TemplateID) {
				klog.Infof("Deleting scaling group %q (uses template %q)", grp.Name, fi.ValueOf(expected.Name))
				if err := asService.DeleteInstanceGroup(&autoscaling.DeleteInstanceGroupRequest{
					Zone:            zone,
					InstanceGroupID: grp.ID,
				}); err != nil {
					return fmt.Errorf("deleting scaling group %q: %w", grp.Name, err)
				}
			}
		}

		klog.Infof("Deleting existing instance template %q for recreation", fi.ValueOf(expected.Name))
		err = asService.DeleteInstanceTemplate(&autoscaling.DeleteInstanceTemplateRequest{
			Zone:       zone,
			TemplateID: fi.ValueOf(actual.TemplateID),
		})
		if err != nil {
			return fmt.Errorf("deleting instance template %q: %w", fi.ValueOf(expected.Name), err)
		}
	}

	{
		klog.Infof("Creating instance template %q", fi.ValueOf(expected.Name))

		projectID, err := target.Cloud.GetProjectID()
		if err != nil {
			return fmt.Errorf("getting project ID: %w", err)
		}

		// The autoscaling API requires at least one volume in the template
		var volumes map[string]*autoscaling.VolumeInstanceTemplate
		templateName := fi.ValueOf(expected.Name)
		if expected.RootVolumeSize != nil {
			sizeGB := uint64(*expected.RootVolumeSize) * 1_000_000_000
			volumes = map[string]*autoscaling.VolumeInstanceTemplate{
				"0": {
					Name:       templateName + "-root",
					Boot:       true,
					VolumeType: autoscaling.VolumeInstanceTemplateVolumeTypeSbs,
					FromEmpty: &autoscaling.VolumeInstanceTemplateFromEmpty{
						Size: scw.Size(sizeGB),
					},
				},
			}
		} else {
			volumes = map[string]*autoscaling.VolumeInstanceTemplate{
				"0": {
					Name:       templateName + "-root",
					Boot:       true,
					VolumeType: autoscaling.VolumeInstanceTemplateVolumeTypeLSSD,
					FromEmpty: &autoscaling.VolumeInstanceTemplateFromEmpty{
						Size: scw.Size(20_000_000_000),
					},
				},
			}
		}

		// Strip region prefix from private network IDs (API expects bare UUIDs)
		var pnIDs []string
		for _, pnID := range expected.PrivateNetworkIDs {
			if parts := strings.SplitN(pnID, "/", 2); len(parts) == 2 {
				pnIDs = append(pnIDs, parts[1])
			} else {
				pnIDs = append(pnIDs, pnID)
			}
		}

		tmpl, err := asService.CreateInstanceTemplate(&autoscaling.CreateInstanceTemplateRequest{
			Zone:              zone,
			ProjectID:         projectID,
			Name:              fi.ValueOf(expected.Name),
			CommercialType:    fi.ValueOf(expected.CommercialType),
			ImageID:           expected.ImageID,
			Tags:              expected.Tags,
			PrivateNetworkIDs: pnIDs,
			PublicIPsV4Count:  fi.PtrTo(uint32(0)),
			CloudInit:         cloudInit,
			Volumes:           volumes,
		})
		if err != nil {
			return fmt.Errorf("creating instance template: %w", err)
		}
		expected.TemplateID = fi.PtrTo(tmpl.ID)
	}

	return nil
}

type terraformInstanceTemplate struct {
	Name              *string  `cty:"name"`
	CommercialType    *string  `cty:"commercial_type"`
	ImageID           *string  `cty:"image_id"`
	Tags              []string `cty:"tags"`
	PrivateNetworkIDs []string `cty:"private_network_ids"`
	CloudInit         *string  `cty:"cloud_init"`
}

func (_ *InstanceTemplate) RenderTerraform(t *terraform.TerraformTarget, actual, expected, changes *InstanceTemplate) error {
	tfName := strings.ReplaceAll(fi.ValueOf(expected.Name), ".", "-")

	tf := terraformInstanceTemplate{
		Name:              expected.Name,
		CommercialType:    expected.CommercialType,
		ImageID:           expected.ImageID,
		Tags:              expected.Tags,
		PrivateNetworkIDs: expected.PrivateNetworkIDs,
	}

	if expected.UserData != nil {
		userData, err := fi.ResourceAsBytes(*expected.UserData)
		if err != nil {
			return err
		}
		encoded := base64.StdEncoding.EncodeToString(userData)
		tf.CloudInit = &encoded
	}

	return t.RenderResource("scaleway_instance_template", tfName, tf)
}

func (t *InstanceTemplate) TerraformLink() *terraformWriter.Literal {
	tfName := strings.ReplaceAll(fi.ValueOf(t.Name), ".", "-")
	return terraformWriter.LiteralProperty("scaleway_instance_template", tfName, "id")
}
