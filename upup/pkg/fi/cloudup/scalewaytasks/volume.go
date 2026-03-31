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

package scalewaytasks

import (
	"fmt"
	"strings"

	block "github.com/scaleway/scaleway-sdk-go/api/block/v1alpha1"
	"github.com/scaleway/scaleway-sdk-go/scw"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/scaleway"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraform"
)

// +kops:fitask
type Volume struct {
	Name      *string
	ID        *string
	Lifecycle fi.Lifecycle

	Size     *int64
	Zone     *string
	Tags     []string
	PerfIops *uint32
}

var _ fi.CompareWithID = (*Volume)(nil)

func (v *Volume) CompareWithID() *string {
	return v.ID
}

func (v *Volume) Find(c *fi.CloudupContext) (*Volume, error) {
	cloud := c.T.Cloud.(scaleway.ScwCloud)
	blockService := cloud.BlockService()
	zone := cloud.Zone()

	volumes, err := blockService.ListVolumes(&block.ListVolumesRequest{
		Name: v.Name,
		Zone: scw.Zone(zone),
	}, scw.WithAllPages())
	if err != nil {
		return nil, err
	}

	for _, volume := range volumes.Volumes {
		if volume.Name == fi.ValueOf(v.Name) {
			found := &Volume{
				Name:      fi.PtrTo(volume.Name),
				ID:        fi.PtrTo(volume.ID),
				Lifecycle: v.Lifecycle,
				Size:      fi.PtrTo(int64(volume.Size)),
				Zone:      fi.PtrTo(string(volume.Zone)),
				Tags:      volume.Tags,
			}
			if volume.Specs != nil {
				found.PerfIops = volume.Specs.PerfIops
			}
			return found, nil
		}
	}

	return nil, nil
}

func (v *Volume) Run(c *fi.CloudupContext) error {
	return fi.CloudupDefaultDeltaRunMethod(v, c)
}

func (_ *Volume) CheckChanges(actual, expected, changes *Volume) error {
	if actual != nil {
		if changes.Name != nil {
			return fi.CannotChangeField("Name")
		}
		if changes.ID != nil {
			return fi.CannotChangeField("ID")
		}
		if changes.Zone != nil {
			return fi.CannotChangeField("Zone")
		}
	} else {
		if expected.Name == nil {
			return fi.RequiredField("Name")
		}
		if expected.Size == nil {
			return fi.RequiredField("Size")
		}
		if expected.Zone == nil {
			return fi.RequiredField("Zone")
		}
	}
	return nil
}

func (_ *Volume) RenderScw(t *scaleway.ScwAPITarget, actual, expected, changes *Volume) error {
	cloud := t.Cloud
	blockService := cloud.BlockService()
	zone := scw.Zone(fi.ValueOf(expected.Zone))

	if actual != nil {
		_, err := blockService.UpdateVolume(&block.UpdateVolumeRequest{
			Zone:     zone,
			VolumeID: fi.ValueOf(actual.ID),
			Name:     expected.Name,
			Tags:     fi.PtrTo(expected.Tags),
			Size:     scw.SizePtr(scw.Size(fi.ValueOf(expected.Size))),
		})
		if err != nil {
			return fmt.Errorf("updating block volume %s (%s): %w", *actual.Name, *actual.ID, err)
		}
	} else {
		projectID, err := cloud.GetProjectID()
		if err != nil {
			return fmt.Errorf("getting project ID for block volume creation: %w", err)
		}

		perfIops := expected.PerfIops
		if perfIops == nil {
			perfIops = fi.PtrTo(uint32(5000))
		}

		_, err = blockService.CreateVolume(&block.CreateVolumeRequest{
			Zone:      zone,
			Name:      fi.ValueOf(expected.Name),
			ProjectID: projectID,
			FromEmpty: &block.CreateVolumeRequestFromEmpty{
				Size: scw.Size(fi.ValueOf(expected.Size)),
			},
			PerfIops: perfIops,
			Tags:     expected.Tags,
		})
		if err != nil {
			return fmt.Errorf("creating block volume: %w", err)
		}
	}

	return nil
}

type terraformVolume struct {
	Name     *string  `cty:"name"`
	SizeInGB *int     `cty:"size_in_gb"`
	Tags     []string `cty:"tags"`
	Boot     *bool    `cty:"boot"`
}

func (_ *Volume) RenderTerraform(t *terraform.TerraformTarget, actual, expected, changes *Volume) error {
	tfName := strings.ReplaceAll(fi.ValueOf(expected.Name), ".", "-")
	tf := &terraformVolume{
		Name:     expected.Name,
		SizeInGB: fi.PtrTo(int(fi.ValueOf(expected.Size) / 1e9)),
		Tags:     expected.Tags,
	}

	return t.RenderResource("scaleway_block_volume", tfName, tf)
}
