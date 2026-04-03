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

package scaleway

import (
	"fmt"
	"os"
	"strings"

	autoscaling "github.com/scaleway/scaleway-sdk-go/api/autoscaling/v1alpha1"
	block "github.com/scaleway/scaleway-sdk-go/api/block/v1alpha1"
	domain "github.com/scaleway/scaleway-sdk-go/api/domain/v2beta1"
	iam "github.com/scaleway/scaleway-sdk-go/api/iam/v1alpha1"
	"github.com/scaleway/scaleway-sdk-go/api/instance/v1"
	ipam "github.com/scaleway/scaleway-sdk-go/api/ipam/v1"
	"github.com/scaleway/scaleway-sdk-go/api/lb/v1"
	"github.com/scaleway/scaleway-sdk-go/api/marketplace/v2"
	"github.com/scaleway/scaleway-sdk-go/scw"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	kopsv "k8s.io/kops"
	"k8s.io/kops/dnsprovider/pkg/dnsprovider"
	dns "k8s.io/kops/dnsprovider/pkg/dnsprovider/providers/scaleway"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/cloudinstances"
	"k8s.io/kops/upup/pkg/fi"
)

const (
	TagClusterName           = "noprefix=kops.k8s.io/cluster"
	TagInstanceGroup         = "noprefix=kops.k8s.io/instance-group"
	TagNameEtcdClusterPrefix = "noprefix=kops.k8s.io/etcd"
	TagNeedsUpdate           = "noprefix=kops.k8s.io/needs-update"
	TagNameRolePrefix        = "noprefix=kops.k8s.io/role"
	TagRoleControlPlane      = "ControlPlane"
	TagRoleWorker            = "Node"
	KopsUserAgentPrefix      = "kubernetes-kops/"
)

// ScwCloud exposes all the interfaces required to operate on Scaleway resources
type ScwCloud interface {
	fi.Cloud

	ClusterName(tags []string) string
	DNS() (dnsprovider.Interface, error)
	ProviderID() kops.CloudProviderID
	Region() string
	Zone() string

	AutoscalingService() *autoscaling.API
	BlockService() *block.API
	DomainService() *domain.API
	IamService() *iam.API
	InstanceService() *instance.API
	IPAMService() *ipam.API
	LBService() *lb.ZonedAPI
	MarketplaceService() *marketplace.API

	DeleteGroup(group *cloudinstances.CloudInstanceGroup) error
	DeleteInstance(i *cloudinstances.CloudInstance) error
	DeregisterInstance(instance *cloudinstances.CloudInstance) error
	DetachInstance(instance *cloudinstances.CloudInstance) error
	FindClusterStatus(cluster *kops.Cluster) (*kops.ClusterStatus, error)
	FindVPCInfo(id string) (*fi.VPCInfo, error)
	GetApiIngressStatus(cluster *kops.Cluster) ([]fi.ApiIngressStatus, error)
	GetCloudGroups(cluster *kops.Cluster, instancegroups []*kops.InstanceGroup, warnUnmatched bool, nodes []v1.Node) (map[string]*cloudinstances.CloudInstanceGroup, error)

	GetClusterBlockVolumes(clusterName string) ([]*block.Volume, error)
	GetClusterDNSRecords(clusterName string) ([]*domain.Record, error)
	GetClusterLoadBalancers(clusterName string) ([]*lb.LB, error)
	GetClusterServers(clusterName string, instanceGroupName *string) ([]*instance.Server, error)
	GetClusterSSHKeys(clusterName string) ([]*iam.SSHKey, error)
	GetClusterVolumes(clusterName string) ([]*instance.Volume, error)
	GetProjectID() (string, error)
	GetServerIP(serverID string, zone scw.Zone) (string, error)

	GetClusterScalingGroups(clusterName string) ([]*autoscaling.InstanceGroup, error)
	GetClusterInstanceTemplates(clusterName string) ([]*autoscaling.InstanceTemplate, error)

	DeleteBlockVolume(volume *block.Volume) error
	DeleteDNSRecord(record *domain.Record, clusterName string) error
	DeleteInstanceScalingGroup(group *autoscaling.InstanceGroup) error
	DeleteInstanceTemplate(template *autoscaling.InstanceTemplate) error
	DeleteLoadBalancer(loadBalancer *lb.LB) error
	DeleteServer(server *instance.Server) error
	DeleteSSHKey(sshkey *iam.SSHKey) error
	DeleteVolume(volume *instance.Volume) error
}

// static compile time check to validate ScwCloud's fi.Cloud Interface.
var _ fi.Cloud = (*scwCloudImplementation)(nil)

// scwCloudImplementation holds the scw.Client object to interact with Scaleway resources.
type scwCloudImplementation struct {
	client *scw.Client
	region scw.Region
	zone   scw.Zone
	dns    dnsprovider.Interface
	tags   map[string]string

	autoscalingAPI *autoscaling.API
	blockAPI       *block.API
	domainAPI      *domain.API
	iamAPI         *iam.API
	instanceAPI    *instance.API
	ipamAPI        *ipam.API
	lbAPI          *lb.ZonedAPI
	marketplaceAPI *marketplace.API
}

// NewScwCloud returns a Cloud with a Scaleway Client using the env vars SCW_PROFILE or
// SCW_ACCESS_KEY, SCW_SECRET_KEY and SCW_DEFAULT_PROJECT_ID
func NewScwCloud(tags map[string]string) (ScwCloud, error) {
	var scwClient *scw.Client
	var region scw.Region
	var zone scw.Zone
	var err error

	if profileName := os.Getenv("SCW_PROFILE"); profileName == "REDACTED" {
		// If the profile is REDACTED, we're running integration tests so no need for authentication
		scwClient, err = scw.NewClient(scw.WithoutAuth())
		if err != nil {
			return nil, err
		}
	} else {
		profile, err := CreateValidScalewayProfile()
		if err != nil {
			return nil, err
		}
		scwClient, err = scw.NewClient(
			scw.WithProfile(profile),
			scw.WithUserAgent(KopsUserAgentPrefix+kopsv.Version),
		)
		if err != nil {
			return nil, fmt.Errorf("creating client for Scaleway Cloud: %w", err)
		}
		region = scw.Region(fi.ValueOf(profile.DefaultRegion))
		zone = scw.Zone(fi.ValueOf(profile.DefaultZone))
	}

	if tags != nil {
		region, err = scw.ParseRegion(tags["region"])
		if err != nil {
			return nil, err
		}
		zone, err = scw.ParseZone(tags["zone"])
		if err != nil {
			return nil, err
		}
	}

	return &scwCloudImplementation{
		client:         scwClient,
		region:         region,
		zone:           zone,
		dns:            dns.NewProvider(domain.NewAPI(scwClient)),
		tags:           tags,
		autoscalingAPI: autoscaling.NewAPI(scwClient),
		blockAPI:       block.NewAPI(scwClient),
		domainAPI:      domain.NewAPI(scwClient),
		iamAPI:         iam.NewAPI(scwClient),
		instanceAPI:    instance.NewAPI(scwClient),
		ipamAPI:        ipam.NewAPI(scwClient),
		lbAPI:          lb.NewZonedAPI(scwClient),
		marketplaceAPI: marketplace.NewAPI(scwClient),
	}, nil
}

func (s *scwCloudImplementation) ClusterName(tags []string) string {
	if tags != nil {
		return ClusterNameFromTags(tags)
	}
	if clusterName, ok := s.tags[TagClusterName]; ok {
		return clusterName
	}
	return ""
}

func (s *scwCloudImplementation) DNS() (dnsprovider.Interface, error) {
	provider, err := dnsprovider.GetDnsProvider(dns.ProviderName, nil)
	if err != nil {
		return nil, fmt.Errorf("error building DNS provider: %w", err)
	}
	return provider, nil
}

func (s *scwCloudImplementation) ProviderID() kops.CloudProviderID {
	return kops.CloudProviderScaleway
}

func (s *scwCloudImplementation) Region() string {
	return string(s.region)
}

func (s *scwCloudImplementation) Zone() string {
	return string(s.zone)
}

func (s *scwCloudImplementation) AutoscalingService() *autoscaling.API {
	return s.autoscalingAPI
}

func (s *scwCloudImplementation) BlockService() *block.API {
	return s.blockAPI
}

func (s *scwCloudImplementation) DomainService() *domain.API {
	return s.domainAPI
}

func (s *scwCloudImplementation) IamService() *iam.API {
	return s.iamAPI
}

func (s *scwCloudImplementation) InstanceService() *instance.API {
	return s.instanceAPI
}

func (s *scwCloudImplementation) IPAMService() *ipam.API {
	return s.ipamAPI
}

func (s *scwCloudImplementation) LBService() *lb.ZonedAPI {
	return s.lbAPI
}

func (s *scwCloudImplementation) MarketplaceService() *marketplace.API {
	return s.marketplaceAPI
}

func (s *scwCloudImplementation) DeleteGroup(group *cloudinstances.CloudInstanceGroup) error {
	toDelete := append(group.NeedUpdate, group.Ready...)
	for _, cloudInstance := range toDelete {
		err := s.DeleteInstance(cloudInstance)
		if err != nil {
			return fmt.Errorf("error deleting group %q: %w", group.HumanName, err)
		}
	}
	return nil
}

func (s *scwCloudImplementation) DeleteInstance(i *cloudinstances.CloudInstance) error {
	server, err := s.instanceAPI.GetServer(&instance.GetServerRequest{
		Zone:     s.zone,
		ServerID: i.ID,
	})
	if err != nil {
		if is404Error(err) {
			klog.V(4).Infof("error deleting cloud instance %s of group %s : instance was already deleted", i.ID, i.CloudInstanceGroup.HumanName)
			return nil
		}
		return fmt.Errorf("deleting cloud instance %s of group %s: %w", i.ID, i.CloudInstanceGroup.HumanName, err)
	}

	err = s.DeleteServer(server.Server)
	if err != nil {
		return fmt.Errorf("deleting cloud instance %s of group %s: %w", i.ID, i.CloudInstanceGroup.HumanName, err)
	}

	return nil
}

func (s *scwCloudImplementation) DeregisterInstance(i *cloudinstances.CloudInstance) error {
	server, err := s.instanceAPI.GetServer(&instance.GetServerRequest{
		Zone:     s.zone,
		ServerID: i.ID,
	})
	if err != nil {
		return fmt.Errorf("deregistering cloud instance %s of group %q: %w", i.ID, i.CloudInstanceGroup.HumanName, err)
	}
	serverIP, err := s.GetServerIP(server.Server.ID, server.Server.Zone)
	if err != nil {
		return fmt.Errorf("deregistering cloud instance %s of group %q: %w", i.ID, i.CloudInstanceGroup.HumanName, err)
	}

	// We remove the instance's IP from load-balancers
	lbs, err := s.GetClusterLoadBalancers(s.ClusterName(server.Server.Tags))
	if err != nil {
		return fmt.Errorf("deregistering cloud instance %s of group %q: %w", i.ID, i.CloudInstanceGroup.HumanName, err)
	}
	for _, loadBalancer := range lbs {
		backEnds, err := s.lbAPI.ListBackends(&lb.ZonedAPIListBackendsRequest{
			Zone: s.zone,
			LBID: loadBalancer.ID,
		}, scw.WithAllPages())
		if err != nil {
			return fmt.Errorf("deregistering cloud instance %s of group %q: listing load-balancer's back-ends for instance creation: %w", i.ID, i.CloudInstanceGroup.HumanName, err)
		}
		for _, backEnd := range backEnds.Backends {
			for _, ip := range backEnd.Pool {
				if ip == serverIP {
					_, err := s.lbAPI.RemoveBackendServers(&lb.ZonedAPIRemoveBackendServersRequest{
						Zone:      s.zone,
						BackendID: backEnd.ID,
						ServerIP:  []string{serverIP},
					})
					if err != nil {
						return fmt.Errorf("deregistering cloud instance %s of group %q: removing IP from lb: %w", i.ID, i.CloudInstanceGroup.HumanName, err)
					}
				}
			}
		}
	}

	return nil
}

func (s *scwCloudImplementation) DetachInstance(i *cloudinstances.CloudInstance) error {
	klog.V(8).Infof("Scaleway DetachInstance is not implemented yet")
	return fmt.Errorf("DetachInstance is not implemented yet for Scaleway")
}

// FindClusterStatus was used before etcd-manager to check the etcd cluster status and prevent unsupported changes.
func (s *scwCloudImplementation) FindClusterStatus(cluster *kops.Cluster) (*kops.ClusterStatus, error) {
	klog.V(8).Info("Scaleway FindClusterStatus is not implemented")
	return nil, nil
}

// FindVPCInfo is not implemented yet, it's only here to satisfy the fi.Cloud interface
func (s *scwCloudImplementation) FindVPCInfo(id string) (*fi.VPCInfo, error) {
	klog.V(8).Info("Scaleway clusters don't have a VPC yet so FindVPCInfo is not implemented")
	return nil, fmt.Errorf("FindVPCInfo is not implemented yet for Scaleway")
}

func (s *scwCloudImplementation) GetApiIngressStatus(cluster *kops.Cluster) ([]fi.ApiIngressStatus, error) {
	var ingresses []fi.ApiIngressStatus
	name := "api." + cluster.Name

	responseLoadBalancers, err := s.lbAPI.ListLBs(&lb.ZonedAPIListLBsRequest{
		Zone: s.zone,
		Name: &name,
	}, scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("finding load-balancers: %w", err)
	}
	if len(responseLoadBalancers.LBs) == 0 {
		klog.V(8).Infof("Could not find any load-balancers for cluster %s", cluster.Name)
		return nil, nil
	}
	if len(responseLoadBalancers.LBs) > 1 {
		klog.V(4).Infof("More than 1 load-balancer with the name %s was found", name)
	}

	for _, loadBalancer := range responseLoadBalancers.LBs {
		for _, lbIP := range loadBalancer.IP {
			ingresses = append(ingresses, fi.ApiIngressStatus{IP: lbIP.IPAddress})
		}
	}

	return ingresses, nil
}

func (s *scwCloudImplementation) GetCloudGroups(cluster *kops.Cluster, instancegroups []*kops.InstanceGroup, warnUnmatched bool, nodes []v1.Node) (map[string]*cloudinstances.CloudInstanceGroup, error) {
	groups := make(map[string]*cloudinstances.CloudInstanceGroup)

	nodeMap := cloudinstances.GetNodeMap(nodes, cluster)

	serverGroups, err := findServerGroups(s, cluster.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to find server groups: %w", err)
	}

	for _, ig := range instancegroups {
		serverGroup, ok := serverGroups[ig.Name]
		if !ok {
			if warnUnmatched {
				klog.Warningf("Server group %q has no corresponding instance group", ig.Name)
			}
			continue
		}

		groups[ig.Name], err = buildCloudGroup(s, ig, serverGroup, nodeMap)
		if err != nil {
			return nil, fmt.Errorf("failed to build cloud group for instance group %q: %w", ig.Name, err)
		}
	}

	return groups, nil
}

func findServerGroups(s *scwCloudImplementation, clusterName string) (map[string][]*instance.Server, error) {
	// First try scaling-group-based discovery: list groups by cluster tag,
	// then find instances by autoscaling_id tag
	scalingGroups, err := s.GetClusterScalingGroups(clusterName)
	if err != nil {
		klog.Warningf("findServerGroups: error listing scaling groups, falling back to legacy discovery: %v", err)
	} else if len(scalingGroups) > 0 {
		return findServerGroupsByScalingGroup(s, scalingGroups)
	} else {
		klog.V(2).Infof("findServerGroups: no scaling groups found for cluster %q", clusterName)
	}

	// Fall back to legacy tag-based discovery
	servers, err := s.GetClusterServers(clusterName, nil)
	if err != nil {
		return nil, err
	}

	serverGroups := make(map[string][]*instance.Server)
	for _, server := range servers {
		igName := InstanceGroupNameFromTags(server.Tags)
		serverGroups[igName] = append(serverGroups[igName], server)
	}

	return serverGroups, nil
}

func findServerGroupsByScalingGroup(s *scwCloudImplementation, scalingGroups []*autoscaling.InstanceGroup) (map[string][]*instance.Server, error) {
	// Build map of scaling group ID → IG name
	groupIDToIGName := make(map[string]string, len(scalingGroups))
	for _, sg := range scalingGroups {
		igName := InstanceGroupNameFromTags(sg.Tags)
		if igName == "" {
			igName = sg.Name
		}
		groupIDToIGName[sg.ID] = igName
	}

	// List all instances and match by autoscaling_id tag
	allServers, err := s.instanceAPI.ListServers(&instance.ListServersRequest{
		Zone: s.zone,
	}, scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("listing servers: %w", err)
	}

	serverGroups := make(map[string][]*instance.Server)
	for _, server := range allServers.Servers {
		asID := autoscalingIDFromTags(server.Tags)
		if igName, ok := groupIDToIGName[asID]; ok {
			serverGroups[igName] = append(serverGroups[igName], server)
		}
	}

	return serverGroups, nil
}

func autoscalingIDFromTags(tags []string) string {
	for _, tag := range tags {
		if strings.HasPrefix(tag, "autoscaling_id:") {
			return strings.TrimPrefix(tag, "autoscaling_id:")
		}
	}
	return ""
}

func buildCloudGroup(s *scwCloudImplementation, ig *kops.InstanceGroup, sg []*instance.Server, nodeMap map[string]*v1.Node) (*cloudinstances.CloudInstanceGroup, error) {
	cloudInstanceGroup := &cloudinstances.CloudInstanceGroup{
		HumanName:     ig.Name,
		InstanceGroup: ig,
		Raw:           sg,
		MinSize:       int(fi.ValueOf(ig.Spec.MinSize)),
		TargetSize:    int(fi.ValueOf(ig.Spec.MinSize)),
		MaxSize:       int(fi.ValueOf(ig.Spec.MaxSize)),
	}

	for _, server := range sg {
		status := cloudinstances.CloudInstanceStatusUpToDate
		for _, tag := range server.Tags {
			if tag == TagNeedsUpdate {
				status = cloudinstances.CloudInstanceStatusNeedsUpdate
			}
		}
		cloudInstance, err := cloudInstanceGroup.NewCloudInstance(server.ID, status, nodeMap[server.ID])
		if err != nil {
			return nil, fmt.Errorf("failed to create cloud instance for server %s(%s): %w", server.Name, server.ID, err)
		}
		cloudInstance.State = cloudinstances.State(server.State)
		cloudInstance.MachineType = server.CommercialType
		// Scaling group instances don't have kOps role tags; derive from IG spec
		role := InstanceRoleFromTags(server.Tags)
		if role == "" {
			if ig.IsControlPlane() {
				role = TagRoleControlPlane
			} else {
				role = "Node"
			}
		}
		cloudInstance.Roles = append(cloudInstance.Roles, role)
		if server.State == instance.ServerStateRunning {
			ip, err := s.GetServerIP(server.ID, server.Zone)
			if err != nil {
				klog.Warningf("getting server IP for %s: %v", server.ID, err)
			} else {
				cloudInstance.PrivateIP = ip
			}
		}
	}

	return cloudInstanceGroup, nil
}

func (s *scwCloudImplementation) GetClusterDNSRecords(clusterName string) ([]*domain.Record, error) {
	names := strings.SplitN(clusterName, ".", 2)
	clusterNameShort := names[0]
	domainName := names[1]

	records, err := s.domainAPI.ListDNSZoneRecords(&domain.ListDNSZoneRecordsRequest{
		DNSZone: domainName,
	}, scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("listing cluster DNS records: %w", err)
	}

	clusterDNSRecords := []*domain.Record(nil)
	for _, record := range records.Records {
		if strings.HasSuffix(record.Name, clusterNameShort) {
			clusterDNSRecords = append(clusterDNSRecords, record)
		}
	}
	return clusterDNSRecords, nil
}

func (s *scwCloudImplementation) GetClusterLoadBalancers(clusterName string) ([]*lb.LB, error) {
	loadBalancerName := "api." + clusterName
	lbs, err := s.lbAPI.ListLBs(&lb.ZonedAPIListLBsRequest{
		Zone: s.zone,
		Name: &loadBalancerName,
	}, scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("listing cluster load-balancers: %w", err)
	}
	return lbs.LBs, nil
}

func (s *scwCloudImplementation) GetClusterServers(clusterName string, instanceGroupName *string) ([]*instance.Server, error) {
	// Try legacy tag-based discovery first
	tags := []string{TagClusterName + "=" + clusterName}
	if instanceGroupName != nil {
		tags = append(tags, fmt.Sprintf("%s=%s", TagInstanceGroup, *instanceGroupName))
	}
	request := &instance.ListServersRequest{
		Zone: s.zone,
		Name: instanceGroupName,
		Tags: tags,
	}
	servers, err := s.instanceAPI.ListServers(request, scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("failed to list cluster servers: %w", err)
	}
	if len(servers.Servers) > 0 {
		return servers.Servers, nil
	}

	// Fall back to scaling-group-based discovery
	scalingGroups, err := s.GetClusterScalingGroups(clusterName)
	if err != nil || len(scalingGroups) == 0 {
		return nil, nil
	}

	groupIDs := make(map[string]string, len(scalingGroups))
	for _, sg := range scalingGroups {
		igName := InstanceGroupNameFromTags(sg.Tags)
		if igName == "" {
			igName = sg.Name
		}
		groupIDs[sg.ID] = igName
	}

	allServers, err := s.instanceAPI.ListServers(&instance.ListServersRequest{
		Zone: s.zone,
	}, scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("listing servers for scaling group discovery: %w", err)
	}

	var matched []*instance.Server
	for _, server := range allServers.Servers {
		asID := autoscalingIDFromTags(server.Tags)
		igName, ok := groupIDs[asID]
		if !ok {
			continue
		}
		if instanceGroupName != nil && igName != *instanceGroupName {
			continue
		}
		matched = append(matched, server)
	}
	return matched, nil
}

func (s *scwCloudImplementation) GetClusterSSHKeys(clusterName string) ([]*iam.SSHKey, error) {
	clusterSSHKeys := []*iam.SSHKey(nil)
	allSSHKeys, err := s.iamAPI.ListSSHKeys(&iam.ListSSHKeysRequest{}, scw.WithAllPages())
	for _, sshkey := range allSSHKeys.SSHKeys {
		if strings.HasPrefix(sshkey.Name, fmt.Sprintf("kubernetes.%s-", clusterName)) {
			clusterSSHKeys = append(clusterSSHKeys, sshkey)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list cluster ssh keys: %w", err)
	}
	return clusterSSHKeys, nil
}

func (s *scwCloudImplementation) GetClusterBlockVolumes(clusterName string) ([]*block.Volume, error) {
	volumes, err := s.blockAPI.ListVolumes(&block.ListVolumesRequest{
		Zone: s.zone,
		Tags: []string{TagClusterName + "=" + clusterName},
	}, scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("listing cluster block volumes: %w", err)
	}
	return volumes.Volumes, nil
}

func (s *scwCloudImplementation) GetClusterVolumes(clusterName string) ([]*instance.Volume, error) {
	volumes, err := s.instanceAPI.ListVolumes(&instance.ListVolumesRequest{
		Zone: s.zone,
		Tags: []string{TagClusterName + "=" + clusterName},
	}, scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("failed to list cluster volumes: %w", err)
	}
	return volumes.Volumes, nil
}

func (s *scwCloudImplementation) GetProjectID() (string, error) {
	projectID, exists := s.client.GetDefaultProjectID()
	if !exists || projectID == "" {
		if os.Getenv("SCW_PROFILE") == "REDACTED" {
			return "00000000-0000-0000-0000-000000000000", nil
		}
		return "", fmt.Errorf("no default project ID found in Scaleway client config")
	}
	return projectID, nil
}

func (s *scwCloudImplementation) GetServerIP(serverID string, zone scw.Zone) (string, error) {
	srv, err := s.instanceAPI.GetServer(&instance.GetServerRequest{
		ServerID: serverID,
		Zone:     zone,
	})
	if err != nil {
		return "", fmt.Errorf("getting server %s: %w", serverID, err)
	}
	if srv.Server.PrivateIP != nil && *srv.Server.PrivateIP != "" {
		return *srv.Server.PrivateIP, nil
	}
	for _, ip := range srv.Server.PublicIPs {
		if ip != nil && ip.Address != nil {
			return ip.Address.String(), nil
		}
	}

	// Fall back to IPAM for private-network-only instances.
	region, err := zone.Region()
	if err != nil {
		return "", fmt.Errorf("no IP found for server %s (unable to parse region: %w)", serverID, err)
	}
	for _, nic := range srv.Server.PrivateNics {
		nicIPs, err := s.ipamAPI.ListIPs(&ipam.ListIPsRequest{
			Region:           region,
			PrivateNetworkID: scw.StringPtr(nic.PrivateNetworkID),
			ResourceID:       scw.StringPtr(nic.ID),
			ResourceType:     ipam.ResourceTypeInstancePrivateNic,
			IsIPv6:           scw.BoolPtr(false),
		}, scw.WithAllPages())
		if err != nil {
			klog.Warningf("GetServerIP: IPAM query for NIC %s failed: %v", nic.ID, err)
			continue
		}
		if nicIPs.TotalCount > 0 {
			return nicIPs.IPs[0].Address.IP.String(), nil
		}
	}

	return "", fmt.Errorf("no IP found for server %s (%d NICs checked via IPAM)", serverID, len(srv.Server.PrivateNics))
}

func (s *scwCloudImplementation) GetClusterScalingGroups(clusterName string) ([]*autoscaling.InstanceGroup, error) {
	groups, err := s.autoscalingAPI.ListInstanceGroups(&autoscaling.ListInstanceGroupsRequest{
		Zone: s.zone,
	}, scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("listing instance groups: %w", err)
	}

	var matched []*autoscaling.InstanceGroup
	for _, grp := range groups.InstanceGroups {
		if ClusterNameFromTags(grp.Tags) == clusterName {
			matched = append(matched, grp)
		}
	}
	return matched, nil
}

func (s *scwCloudImplementation) GetClusterInstanceTemplates(clusterName string) ([]*autoscaling.InstanceTemplate, error) {
	templates, err := s.autoscalingAPI.ListInstanceTemplates(&autoscaling.ListInstanceTemplatesRequest{
		Zone: s.zone,
	}, scw.WithAllPages())
	if err != nil {
		return nil, fmt.Errorf("listing instance templates: %w", err)
	}

	var matched []*autoscaling.InstanceTemplate
	for _, tmpl := range templates.InstanceTemplates {
		if ClusterNameFromTags(tmpl.Tags) == clusterName {
			matched = append(matched, tmpl)
		}
	}
	return matched, nil
}

func (s *scwCloudImplementation) DeleteInstanceScalingGroup(group *autoscaling.InstanceGroup) error {
	err := s.autoscalingAPI.DeleteInstanceGroup(&autoscaling.DeleteInstanceGroupRequest{
		Zone:            s.zone,
		InstanceGroupID: group.ID,
	})
	if err != nil {
		if is404Error(err) {
			klog.V(8).Infof("Instance group %q (%s) was already deleted", group.Name, group.ID)
			return nil
		}
		return fmt.Errorf("deleting instance group %s(%s): %w", group.Name, group.ID, err)
	}
	return nil
}

func (s *scwCloudImplementation) DeleteInstanceTemplate(template *autoscaling.InstanceTemplate) error {
	err := s.autoscalingAPI.DeleteInstanceTemplate(&autoscaling.DeleteInstanceTemplateRequest{
		Zone:       s.zone,
		TemplateID: template.ID,
	})
	if err != nil {
		if is404Error(err) {
			klog.V(8).Infof("Instance template %q (%s) was already deleted", template.Name, template.ID)
			return nil
		}
		return fmt.Errorf("deleting instance template %s(%s): %w", template.Name, template.ID, err)
	}
	return nil
}

func (s *scwCloudImplementation) DeleteBlockVolume(volume *block.Volume) error {
	err := s.blockAPI.DeleteVolume(&block.DeleteVolumeRequest{
		VolumeID: volume.ID,
		Zone:     s.zone,
	})
	if err != nil {
		if is404Error(err) {
			klog.V(8).Infof("Block volume %q (%s) was already deleted", volume.Name, volume.ID)
			return nil
		}
		return fmt.Errorf("deleting block volume %s: %w", volume.ID, err)
	}

	_, err = s.blockAPI.WaitForVolume(&block.WaitForVolumeRequest{
		VolumeID: volume.ID,
		Zone:     s.zone,
	})
	if err != nil && !is404Error(err) {
		return fmt.Errorf("delete block volume %s: waiting for volume after deletion: %w", volume.ID, err)
	}

	return nil
}

func (s *scwCloudImplementation) DeleteDNSRecord(record *domain.Record, clusterName string) error {
	domainName := strings.SplitN(clusterName, ".", 2)[1]
	recordDeleteRequest := &domain.UpdateDNSZoneRecordsRequest{
		DNSZone: domainName,
		Changes: []*domain.RecordChange{
			{
				Delete: &domain.RecordChangeDelete{
					ID: scw.StringPtr(record.ID),
				},
			},
		},
	}
	_, err := s.domainAPI.UpdateDNSZoneRecords(recordDeleteRequest)
	if err != nil {
		if is404Error(err) {
			klog.V(8).Infof("DNS record %q (%s) was already deleted", record.Name, record.ID)
			return nil
		}
		return fmt.Errorf("failed to delete record %s: %w", record.Name, err)
	}
	return nil
}

func (s *scwCloudImplementation) DeleteLoadBalancer(loadBalancer *lb.LB) error {
	ipsToRelease := loadBalancer.IP

	// We delete the load-balancer once it's in a stable state
	_, err := s.lbAPI.WaitForLb(&lb.ZonedAPIWaitForLBRequest{
		LBID: loadBalancer.ID,
		Zone: s.zone,
	})
	if err != nil {
		if is404Error(err) {
			klog.V(8).Infof("Load-balancer %q (%s) was already deleted", loadBalancer.Name, loadBalancer.ID)
			return nil
		}
		return fmt.Errorf("waiting for load-balancer: %w", err)
	}
	err = s.lbAPI.DeleteLB(&lb.ZonedAPIDeleteLBRequest{
		Zone: s.zone,
		LBID: loadBalancer.ID,
	})
	if err != nil {
		return fmt.Errorf("deleting load-balancer %s: %w", loadBalancer.ID, err)
	}

	// We wait for the load-balancer to be deleted, then we detach its IPs
	_, err = s.lbAPI.WaitForLb(&lb.ZonedAPIWaitForLBRequest{
		LBID: loadBalancer.ID,
		Zone: s.zone,
	})
	if !is404Error(err) {
		return fmt.Errorf("waiting for load-balancer %s after deletion: %w", loadBalancer.ID, err)
	}
	for _, ip := range ipsToRelease {
		err := s.lbAPI.ReleaseIP(&lb.ZonedAPIReleaseIPRequest{
			Zone: s.zone,
			IPID: ip.ID,
		})
		if err != nil {
			return fmt.Errorf("deleting load-balancer IP: %w", err)
		}
	}
	return nil
}

func (s *scwCloudImplementation) DeleteServer(server *instance.Server) error {
	srv, err := s.instanceAPI.GetServer(&instance.GetServerRequest{
		Zone:     s.zone,
		ServerID: server.ID,
	})
	if err != nil {
		if is404Error(err) {
			klog.V(4).Infof("delete server %s: instance %q was already deleted", server.ID, server.Name)
			return nil
		}
		return err
	}

	// We detach the etcd volumes
	for _, volume := range srv.Server.Volumes {
		if volume.VolumeType == instance.VolumeServerVolumeTypeSbsVolume {
			blockVolume, err := s.blockAPI.GetVolume(&block.GetVolumeRequest{
				Zone:     s.zone,
				VolumeID: volume.ID,
			})
			if err != nil {
				if is404Error(err) {
					continue
				}
				return fmt.Errorf("delete server %s: getting block volume info for %s: %w", server.ID, volume.ID, err)
			}
			for _, tag := range blockVolume.Tags {
				if strings.HasPrefix(tag, TagNameEtcdClusterPrefix) {
					_, err = s.instanceAPI.DetachServerVolume(&instance.DetachServerVolumeRequest{
						Zone:     s.zone,
						ServerID: server.ID,
						VolumeID: volume.ID,
					})
					if err != nil {
						return fmt.Errorf("delete server %s: detaching block volume %s: %w", server.ID, volume.ID, err)
					}
					break
				}
			}
		} else {
			volumeResponse, err := s.instanceAPI.GetVolume(&instance.GetVolumeRequest{
				Zone:     s.zone,
				VolumeID: volume.ID,
			})
			if err != nil {
				if is404Error(err) {
					continue
				}
				return fmt.Errorf("delete server %s: getting infos for volume %s: %w", server.ID, volume.ID, err)
			}
			for _, tag := range volumeResponse.Volume.Tags {
				if strings.HasPrefix(tag, TagNameEtcdClusterPrefix) {
					_, err = s.instanceAPI.DetachVolume(&instance.DetachVolumeRequest{
						Zone:     s.zone,
						VolumeID: volume.ID,
					})
					if err != nil {
						return fmt.Errorf("delete server %s: detaching volume %s: %w", server.ID, volume.ID, err)
					}
					break
				}
			}
		}
	}

	// We terminate the server. This stops and deletes the machine immediately
	_, err = s.instanceAPI.ServerAction(&instance.ServerActionRequest{
		Zone:     s.zone,
		ServerID: server.ID,
		Action:   instance.ServerActionTerminate,
	})
	if err != nil && !is404Error(err) {
		return fmt.Errorf("delete server %s: terminating instance: %w", server.ID, err)
	}

	_, err = s.instanceAPI.WaitForServer(&instance.WaitForServerRequest{
		ServerID: server.ID,
		Zone:     s.zone,
	})
	if err != nil && !is404Error(err) {
		return fmt.Errorf("delete server %s: waiting for instance after termination: %w", server.ID, err)
	}

	return nil
}

func (s *scwCloudImplementation) DeleteSSHKey(sshkey *iam.SSHKey) error {
	err := s.iamAPI.DeleteSSHKey(&iam.DeleteSSHKeyRequest{
		SSHKeyID: sshkey.ID,
	})
	if err != nil {
		if is404Error(err) {
			klog.V(8).Infof("SSH key %q (%s) was already deleted", sshkey.Name, sshkey.ID)
			return nil
		}
		return fmt.Errorf("failed to delete ssh key %s: %w", sshkey.ID, err)
	}
	return nil
}

func (s *scwCloudImplementation) DeleteVolume(volume *instance.Volume) error {
	err := s.instanceAPI.DeleteVolume(&instance.DeleteVolumeRequest{
		VolumeID: volume.ID,
		Zone:     s.zone,
	})
	if err != nil {
		if is404Error(err) {
			klog.V(8).Infof("Volume %q (%s) was already deleted", volume.Name, volume.ID)
			return nil
		}
		return fmt.Errorf("failed to delete volume %s: %w", volume.ID, err)
	}

	_, err = s.instanceAPI.WaitForVolume(&instance.WaitForVolumeRequest{
		VolumeID: volume.ID,
		Zone:     s.zone,
	})
	if !is404Error(err) {
		return fmt.Errorf("delete volume %s: error waiting for volume after deletion: %w", volume.ID, err)
	}

	return nil
}
