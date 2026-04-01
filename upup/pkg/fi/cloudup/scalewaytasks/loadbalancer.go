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
	"os"
	"strings"

	"k8s.io/klog/v2"
	"k8s.io/kops/pkg/wellknownservices"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/scaleway"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraform"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraformWriter"

	"github.com/scaleway/scaleway-sdk-go/api/lb/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
)

const LbDefaultType = "LB-S"

// +kops:fitask
type LoadBalancer struct {
	Name      *string
	Type      string
	Lifecycle fi.Lifecycle

	Zone                  *string
	LBID                  *string
	LBAddresses           []string
	Tags                  []string
	Description           string
	SslCompatibilityLevel string

	// PrivateNetworkID attaches the LB to a private network so it can
	// reach backend instances that have no public IP.
	PrivateNetworkID *string

	// WellKnownServices indicates which services are supported by this resource.
	// This field is internal and is not rendered to the cloud.
	WellKnownServices []wellknownservices.WellKnownService
}

var (
	_ fi.CompareWithID = (*LoadBalancer)(nil)
	_ fi.HasAddress    = (*LoadBalancer)(nil)
)

func (l *LoadBalancer) CompareWithID() *string {
	return l.LBID
}

// GetWellKnownServices implements fi.HasAddress::GetWellKnownServices.
// It indicates which services we support with this load balancer.
func (l *LoadBalancer) GetWellKnownServices() []wellknownservices.WellKnownService {
	return l.WellKnownServices
}

func (l *LoadBalancer) Find(context *fi.CloudupContext) (*LoadBalancer, error) {
	cloud := context.T.Cloud.(scaleway.ScwCloud)
	lbService := cloud.LBService()

	lbResponse, err := lbService.ListLBs(&lb.ZonedAPIListLBsRequest{
		Zone: scw.Zone(cloud.Zone()),
		Name: l.Name,
	}, scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("getting load-balancer %s: %w", fi.ValueOf(l.LBID), err)
	}
	if lbResponse.TotalCount != 1 {
		return nil, nil
	}
	loadBalancer := lbResponse.LBs[0]

	lbIPs := []string(nil)
	for _, IP := range loadBalancer.IP {
		lbIPs = append(lbIPs, IP.IPAddress)
	}

	found := &LoadBalancer{
		Name:              fi.PtrTo(loadBalancer.Name),
		LBID:              fi.PtrTo(loadBalancer.ID),
		Zone:              fi.PtrTo(string(loadBalancer.Zone)),
		LBAddresses:       lbIPs,
		Tags:              loadBalancer.Tags,
		Lifecycle:         l.Lifecycle,
		WellKnownServices: l.WellKnownServices,
	}

	// Check if the LB already has a private network attached (only if expected)
	if l.PrivateNetworkID != nil {
		pnResp, err := lbService.ListLBPrivateNetworks(&lb.ZonedAPIListLBPrivateNetworksRequest{
			Zone: loadBalancer.Zone,
			LBID: loadBalancer.ID,
		})
		if err != nil {
			return nil, fmt.Errorf("listing private networks for load-balancer %s: %w", loadBalancer.ID, err)
		}
		for _, pn := range pnResp.PrivateNetwork {
			if pn.Status == lb.PrivateNetworkStatusReady || pn.Status == lb.PrivateNetworkStatusPending {
				found.PrivateNetworkID = fi.PtrTo(pn.PrivateNetworkID)
				break
			}
		}
	}

	return found, nil
}

func (l *LoadBalancer) FindAddresses(context *fi.CloudupContext) ([]string, error) {
	// Skip if we're running integration tests
	if profileName := os.Getenv("SCW_PROFILE"); profileName == "REDACTED" {
		return nil, nil
	}

	cloud := context.T.Cloud.(scaleway.ScwCloud)
	lbService := cloud.LBService()

	loadBalancers, err := lbService.ListLBs(&lb.ZonedAPIListLBsRequest{
		Zone: scw.Zone(cloud.Zone()),
		Name: l.Name,
	})
	if err != nil {
		return nil, err
	}

	addresses := []string(nil)
	for _, loadBalancer := range loadBalancers.LBs {
		for _, address := range loadBalancer.IP {
			addresses = append(addresses, address.IPAddress)
		}
	}

	return addresses, nil
}

func (l *LoadBalancer) Run(context *fi.CloudupContext) error {
	return fi.CloudupDefaultDeltaRunMethod(l, context)
}

func (_ *LoadBalancer) CheckChanges(actual, expected, changes *LoadBalancer) error {
	if actual != nil {
		if changes.Name != nil {
			return fi.CannotChangeField("Name")
		}
		if changes.LBID != nil {
			return fi.CannotChangeField("ID")
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
	}
	return nil
}

func (l *LoadBalancer) RenderScw(t *scaleway.ScwAPITarget, actual, expected, changes *LoadBalancer) error {
	lbService := t.Cloud.LBService()

	if actual != nil {

		klog.Infof("Updating existing load-balancer with name %q", fi.ValueOf(expected.Name))

		// We update the tags
		if changes != nil || len(actual.Tags) != len(expected.Tags) {
			_, err := lbService.UpdateLB(&lb.ZonedAPIUpdateLBRequest{
				Zone:                  scw.Zone(fi.ValueOf(actual.Zone)),
				LBID:                  fi.ValueOf(actual.LBID),
				Name:                  fi.ValueOf(actual.Name),
				Description:           expected.Description,
				SslCompatibilityLevel: lb.SSLCompatibilityLevel(expected.SslCompatibilityLevel),
				Tags:                  expected.Tags,
			})
			if err != nil {
				return fmt.Errorf("updatings tags for load-balancer %q: %w", fi.ValueOf(expected.Name), err)
			}
		}

		expected.LBID = actual.LBID
		expected.LBAddresses = actual.LBAddresses

	} else {

		klog.Infof("Creating new load-balancer with name %q", fi.ValueOf(expected.Name))

		lbCreated, err := lbService.CreateLB(&lb.ZonedAPICreateLBRequest{
			Zone: scw.Zone(fi.ValueOf(expected.Zone)),
			Name: fi.ValueOf(expected.Name),
			Type: LbDefaultType,
			Tags: expected.Tags,
		})
		if err != nil {
			return fmt.Errorf("creating load-balancer: %w", err)
		}

		_, err = lbService.WaitForLb(&lb.ZonedAPIWaitForLBRequest{
			LBID: lbCreated.ID,
			Zone: scw.Zone(fi.ValueOf(expected.Zone)),
		})
		if err != nil {
			return fmt.Errorf("waiting for load-balancer %s: %w", lbCreated.ID, err)
		}

		lbIPs := []string(nil)
		for _, ip := range lbCreated.IP {
			lbIPs = append(lbIPs, ip.IPAddress)
		}
		expected.LBID = &lbCreated.ID
		expected.LBAddresses = lbIPs

	}

	// Attach private network if configured and not already attached
	if expected.PrivateNetworkID != nil {
		pnID := fi.ValueOf(expected.PrivateNetworkID)
		if parts := strings.SplitN(pnID, "/", 2); len(parts) == 2 {
			pnID = parts[1]
		}

		alreadyAttached := false
		if actual != nil && actual.PrivateNetworkID != nil {
			alreadyAttached = true
		}

		if !alreadyAttached {
			zone := scw.Zone(fi.ValueOf(expected.Zone))
			klog.Infof("Attaching load-balancer %q to private network %s", fi.ValueOf(expected.Name), pnID)

			_, err := lbService.AttachPrivateNetwork(&lb.ZonedAPIAttachPrivateNetworkRequest{
				Zone:             zone,
				LBID:             fi.ValueOf(expected.LBID),
				PrivateNetworkID: pnID,
			})
			if err != nil {
				return fmt.Errorf("attaching load-balancer to private network %s: %w", pnID, err)
			}

			_, err = lbService.WaitForLBPN(&lb.ZonedAPIWaitForLBPNRequest{
				LBID: fi.ValueOf(expected.LBID),
				Zone: zone,
			})
			if err != nil {
				return fmt.Errorf("waiting for load-balancer private network attachment: %w", err)
			}
		}
	}

	return nil
}

type terraformLBIP struct{}

type terraformLoadBalancer struct {
	Type        string                   `cty:"type"`
	Name        *string                  `cty:"name"`
	Description string                   `cty:"description"`
	Tags        []string                 `cty:"tags"`
	IPID        *terraformWriter.Literal `cty:"ip_id"`
}

type terraformLBPrivateNetwork struct {
	LBID             *terraformWriter.Literal `cty:"lb_id"`
	PrivateNetworkID *string                  `cty:"private_network_id"`
}

func (_ *LoadBalancer) RenderTerraform(t *terraform.TerraformTarget, actual, expected, changes *LoadBalancer) error {
	tfName := strings.ReplaceAll(fi.ValueOf(expected.Name), ".", "-")

	tfLBIP := terraformLBIP{}
	err := t.RenderResource("scaleway_lb_ip", tfName, tfLBIP)
	if err != nil {
		return err
	}

	tfLB := terraformLoadBalancer{
		Type:        LbDefaultType,
		Name:        expected.Name,
		Description: expected.Description,
		Tags:        expected.Tags,
		IPID:        terraformWriter.LiteralProperty("scaleway_lb_ip", tfName, "id"),
	}
	err = t.RenderResource("scaleway_lb", tfName, tfLB)
	if err != nil {
		return err
	}

	if expected.PrivateNetworkID != nil {
		tfPN := terraformLBPrivateNetwork{
			LBID:             terraformWriter.LiteralProperty("scaleway_lb", tfName, "id"),
			PrivateNetworkID: expected.PrivateNetworkID,
		}
		err = t.RenderResource("scaleway_lb_private_network", tfName, tfPN)
		if err != nil {
			return err
		}
	}

	return nil
}

func (l *LoadBalancer) TerraformLink() *terraformWriter.Literal {
	return terraformWriter.LiteralProperty("scaleway_lb", fi.ValueOf(l.Name), "id")
}
