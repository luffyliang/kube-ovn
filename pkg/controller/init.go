package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/scylladb/go-set/strset"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/util"
)

func (c *Controller) InitOVN() error {
	if err := c.initClusterRouter(); err != nil {
		klog.Errorf("init cluster router failed: %v", err)
		return err
	}

	if err := c.initDefaultVlan(); err != nil {
		klog.Errorf("init default vlan failed: %v", err)
		return err
	}

	if err := c.initNodeSwitch(); err != nil {
		klog.Errorf("init node switch failed: %v", err)
		return err
	}

	if err := c.initDefaultLogicalSwitch(); err != nil {
		klog.Errorf("init default switch failed: %v", err)
		return err
	}

	if err := c.initHtbQos(); err != nil {
		klog.Errorf("init default qos failed: %v", err)
		return err
	}

	return nil
}

func (c *Controller) InitDefaultVpc() error {
	orivpc, err := c.vpcsLister.Get(util.DefaultVpc)
	if err != nil {
		orivpc = &kubeovnv1.Vpc{}
		orivpc.Name = util.DefaultVpc
		orivpc, err = c.config.KubeOvnClient.KubeovnV1().Vpcs().Create(context.Background(), orivpc, metav1.CreateOptions{})
		if err != nil {
			klog.Errorf("init default vpc failed: %v", err)
			return err
		}
	}
	vpc := orivpc.DeepCopy()
	vpc.Status.DefaultLogicalSwitch = c.config.DefaultLogicalSwitch
	vpc.Status.Router = c.config.ClusterRouter
	vpc.Status.Standby = true
	vpc.Status.Default = true

	bytes, err := vpc.Status.Bytes()
	if err != nil {
		return err
	}
	_, err = c.config.KubeOvnClient.KubeovnV1().Vpcs().Patch(context.Background(), vpc.Name, types.MergePatchType, bytes, metav1.PatchOptions{}, "status")
	if err != nil {
		klog.Errorf("init default vpc failed: %v", err)
		return err
	}
	return nil
}

// InitDefaultLogicalSwitch init the default logical switch for ovn network
func (c *Controller) initDefaultLogicalSwitch() error {
	subnet, err := c.config.KubeOvnClient.KubeovnV1().Subnets().Get(context.Background(), c.config.DefaultLogicalSwitch, metav1.GetOptions{})
	if err == nil {
		if subnet != nil && util.CheckProtocol(c.config.DefaultCIDR) != util.CheckProtocol(subnet.Spec.CIDRBlock) {
			// single-stack upgrade to dual-stack
			if util.CheckProtocol(c.config.DefaultCIDR) == kubeovnv1.ProtocolDual {
				subnet := subnet.DeepCopy()
				subnet.Spec.CIDRBlock = c.config.DefaultCIDR
				if err := formatSubnet(subnet, c); err != nil {
					klog.Errorf("init format subnet %s failed: %v", c.config.DefaultLogicalSwitch, err)
					return err
				}
			}
		}
		return nil
	}

	if !k8serrors.IsNotFound(err) {
		klog.Errorf("get default subnet %s failed: %v", c.config.DefaultLogicalSwitch, err)
		return err
	}

	defaultSubnet := kubeovnv1.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: c.config.DefaultLogicalSwitch},
		Spec: kubeovnv1.SubnetSpec{
			Vpc:                 util.DefaultVpc,
			Default:             true,
			Provider:            util.OvnProvider,
			CIDRBlock:           c.config.DefaultCIDR,
			Gateway:             c.config.DefaultGateway,
			DisableGatewayCheck: !c.config.DefaultGatewayCheck,
			ExcludeIps:          strings.Split(c.config.DefaultExcludeIps, ","),
			NatOutgoing:         true,
			GatewayType:         kubeovnv1.GWDistributedType,
			Protocol:            util.CheckProtocol(c.config.DefaultCIDR),
		},
	}
	if c.config.NetworkType == util.NetworkTypeVlan {
		defaultSubnet.Spec.Vlan = c.config.DefaultVlanName
		defaultSubnet.Spec.LogicalGateway = c.config.DefaultLogicalGateway
	}

	_, err = c.config.KubeOvnClient.KubeovnV1().Subnets().Create(context.Background(), &defaultSubnet, metav1.CreateOptions{})
	return err
}

// InitNodeSwitch init node switch to connect host and pod
func (c *Controller) initNodeSwitch() error {
	subnet, err := c.config.KubeOvnClient.KubeovnV1().Subnets().Get(context.Background(), c.config.NodeSwitch, metav1.GetOptions{})
	if err == nil {
		if subnet != nil && util.CheckProtocol(c.config.NodeSwitchCIDR) != util.CheckProtocol(subnet.Spec.CIDRBlock) {
			// single-stack upgrade to dual-stack
			if util.CheckProtocol(c.config.NodeSwitchCIDR) == kubeovnv1.ProtocolDual {
				subnet := subnet.DeepCopy()
				subnet.Spec.CIDRBlock = c.config.NodeSwitchCIDR
				if err := formatSubnet(subnet, c); err != nil {
					klog.Errorf("init format subnet %s failed: %v", c.config.NodeSwitch, err)
					return err
				}
			}
		}
		return nil
	}

	if !k8serrors.IsNotFound(err) {
		klog.Errorf("get node subnet %s failed: %v", c.config.NodeSwitch, err)
		return err
	}

	nodeSubnet := kubeovnv1.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: c.config.NodeSwitch},
		Spec: kubeovnv1.SubnetSpec{
			Vpc:                    util.DefaultVpc,
			Default:                false,
			Provider:               util.OvnProvider,
			CIDRBlock:              c.config.NodeSwitchCIDR,
			Gateway:                c.config.NodeSwitchGateway,
			ExcludeIps:             strings.Split(c.config.NodeSwitchGateway, ","),
			Protocol:               util.CheckProtocol(c.config.NodeSwitchCIDR),
			DisableInterConnection: true,
		},
	}

	_, err = c.config.KubeOvnClient.KubeovnV1().Subnets().Create(context.Background(), &nodeSubnet, metav1.CreateOptions{})
	return err
}

// InitClusterRouter init cluster router to connect different logical switches
func (c *Controller) initClusterRouter() error {
	lrs, err := c.ovnClient.ListLogicalRouter(c.config.EnableExternalVpc, nil)
	if err != nil {
		return err
	}
	klog.Infof("exists routers: %v", lrs)
	for _, r := range lrs {
		if c.config.ClusterRouter == r.Name {
			return nil
		}
	}
	return c.ovnClient.CreateLogicalRouter(c.config.ClusterRouter)
}

func (c *Controller) InitIPAM() error {
	start := time.Now()
	subnets, err := c.subnetsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list subnet: %v", err)
		return err
	}
	for _, subnet := range subnets {
		if err := c.ipam.AddOrUpdateSubnet(subnet.Name, subnet.Spec.CIDRBlock, subnet.Spec.ExcludeIps); err != nil {
			klog.Errorf("failed to init subnet %s: %v", subnet.Name, err)
		}
	}

	lsList, err := c.ovnClient.ListLogicalSwitch(false, nil)
	if err != nil {
		klog.Errorf("failed to list LS: %v", err)
		return err
	}
	lsPortsMap := make(map[string]*strset.Set, len(lsList))
	for _, ls := range lsList {
		lsPortsMap[ls.Name] = strset.New(ls.Ports...)
	}

	lspList, err := c.ovnClient.ListLogicalSwitchPortsWithLegacyExternalIDs()
	if err != nil {
		klog.Errorf("failed to list LSP: %v", err)
		return err
	}
	lspWithoutVendor := strset.NewWithSize(len(lspList))
	lspWithoutLS := make(map[string]string, len(lspList))
	for _, lsp := range lspList {
		if len(lsp.ExternalIDs) == 0 || lsp.ExternalIDs["vendor"] == "" {
			lspWithoutVendor.Add(lsp.Name)
		}
		if len(lsp.ExternalIDs) == 0 || lsp.ExternalIDs[logicalSwitchKey] == "" {
			lspWithoutLS[lsp.Name] = lsp.UUID
		}
	}

	pods, err := c.podsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list pods: %v", err)
		return err
	}
	for _, pod := range pods {
		if isPodAlive(pod) && pod.Annotations[util.AllocatedAnnotation] == "true" {
			podNets, err := c.getPodKubeovnNets(pod)
			if err != nil {
				klog.Errorf("failed to get pod kubeovn nets %s.%s address %s: %v", pod.Name, pod.Namespace, pod.Annotations[util.IpAddressAnnotation], err)
			}
			for _, podNet := range podNets {
				_, _, _, err := c.ipam.GetStaticAddress(
					fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
					ovs.PodNameToPortName(pod.Name, pod.Namespace, podNet.ProviderName),
					pod.Annotations[fmt.Sprintf(util.IpAddressAnnotationTemplate, podNet.ProviderName)],
					pod.Annotations[fmt.Sprintf(util.MacAddressAnnotationTemplate, podNet.ProviderName)],
					pod.Annotations[fmt.Sprintf(util.LogicalSwitchAnnotationTemplate, podNet.ProviderName)], false)
				if err != nil {
					klog.Errorf("failed to init pod %s.%s address %s: %v", pod.Name, pod.Namespace, pod.Annotations[util.IpAddressAnnotation], err)
				}
				if podNet.ProviderName == util.OvnProvider || strings.HasSuffix(podNet.ProviderName, util.OvnProvider) {
					portName := ovs.PodNameToPortName(pod.Name, pod.Namespace, podNet.ProviderName)
					externalIDs := make(map[string]string, 3)
					if lspWithoutVendor.Has(portName) {
						externalIDs["vendor"] = util.CniTypeName
						externalIDs["pod"] = fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
					}
					if uuid := lspWithoutLS[portName]; uuid != "" {
						for ls, ports := range lsPortsMap {
							if ports.Has(uuid) {
								externalIDs[logicalSwitchKey] = ls
								break
							}
						}
					}

					if err = c.initAppendLspExternalIds(portName, externalIDs); err != nil {
						klog.Errorf("failed to append external-ids for logical switch port %s: %v", portName, err)
					}
				}

			}
		}
	}

	ips, err := c.ipsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list IPs: %v", err)
		return err
	}
	for _, ip := range ips {
		var ipamKey string
		if ip.Spec.Namespace != "" {
			// If there is no pod, clear ip resources. Just for ECX
			_, err := c.podsLister.Pods(ip.Spec.Namespace).Get(ip.Spec.PodName)
			if err != nil && k8serrors.IsNotFound(err) {
				if err := c.config.KubeOvnClient.KubeovnV1().IPs().Delete(context.Background(), ip.Name, metav1.DeleteOptions{}); err != nil {
					klog.Errorf("failed to delete IP CR %s: %v", ip.Name, err)
				} else {
					c.ipam.ReleaseAddressByPod(fmt.Sprintf("%s/%s", ip.Spec.Namespace, ip.Spec.PodName))
				}
				continue
			}
			ipamKey = fmt.Sprintf("%s/%s", ip.Spec.Namespace, ip.Spec.PodName)
		} else {
			ipamKey = fmt.Sprintf("node-%s", ip.Spec.PodName)
		}
		if _, _, _, err = c.ipam.GetStaticAddress(ipamKey, ip.Name, ip.Spec.IPAddress, ip.Spec.MacAddress, ip.Spec.Subnet, false); err != nil {
			klog.Errorf("failed to init IPAM from IP CR %s: %v", ip.Name, err)
		}
		for i := range ip.Spec.AttachSubnets {
			if i == len(ip.Spec.AttachIPs) || i == len(ip.Spec.AttachMacs) {
				klog.Errorf("attachment IP/MAC of IP CR %s is invalid", ip.Name)
				break
			}
			if _, _, _, err = c.ipam.GetStaticAddress(ipamKey, ip.Name, ip.Spec.AttachIPs[i], ip.Spec.AttachMacs[i], ip.Spec.AttachSubnets[i], false); err != nil {
				klog.Errorf("failed to init IPAM from IP CR %s: %v", ip.Name, err)
			}
		}
	}

	nodes, err := c.nodesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list nodes: %v", err)
		return err
	}
	for _, node := range nodes {
		if node.Annotations[util.AllocatedAnnotation] == "true" {
			portName := fmt.Sprintf("node-%s", node.Name)
			v4IP, v6IP, _, err := c.ipam.GetStaticAddress(portName, portName, node.Annotations[util.IpAddressAnnotation],
				node.Annotations[util.MacAddressAnnotation],
				node.Annotations[util.LogicalSwitchAnnotation], true)
			if err != nil {
				klog.Errorf("failed to init node %s.%s address %s: %v", node.Name, node.Namespace, node.Annotations[util.IpAddressAnnotation], err)
			}
			if v4IP != "" && v6IP != "" {
				node.Annotations[util.IpAddressAnnotation] = util.GetStringIP(v4IP, v6IP)
			}

			externalIDs := make(map[string]string, 2)
			if lspWithoutVendor.Has(portName) {
				externalIDs["vendor"] = util.CniTypeName
			}
			if uuid := lspWithoutLS[portName]; uuid != "" {
				for ls, ports := range lsPortsMap {
					if ports.Has(uuid) {
						externalIDs[logicalSwitchKey] = ls
						break
					}
				}
			}

			if err = c.initAppendLspExternalIds(portName, externalIDs); err != nil {
				klog.Errorf("failed to append external-ids for logical switch port %s: %v", portName, err)
			}
		}
	}
	klog.Infof("take %.2f seconds to initialize IPAM", time.Since(start).Seconds())
	return nil
}

func (c *Controller) initDefaultProviderNetwork() error {
	_, err := c.providerNetworksLister.Get(c.config.DefaultProviderName)
	if err == nil {
		return nil
	}
	if !k8serrors.IsNotFound(err) {
		klog.Errorf("failed to get default provider network %s: %v", c.config.DefaultProviderName, err)
		return err
	}

	pn := kubeovnv1.ProviderNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name: c.config.DefaultProviderName,
		},
		Spec: kubeovnv1.ProviderNetworkSpec{
			DefaultInterface: c.config.DefaultHostInterface,
		},
	}

	_, err = c.config.KubeOvnClient.KubeovnV1().ProviderNetworks().Create(context.Background(), &pn, metav1.CreateOptions{})
	return err
}

func (c *Controller) initDefaultVlan() error {
	if c.config.NetworkType != util.NetworkTypeVlan {
		return nil
	}

	if err := c.initDefaultProviderNetwork(); err != nil {
		return err
	}

	_, err := c.vlansLister.Get(c.config.DefaultVlanName)
	if err == nil {
		return nil
	}

	if !k8serrors.IsNotFound(err) {
		klog.Errorf("get default vlan %s failed: %v", c.config.DefaultVlanName, err)
		return err
	}

	if c.config.DefaultVlanID < 0 || c.config.DefaultVlanID > 4095 {
		return fmt.Errorf("the default vlan id is not between 1-4095")
	}

	defaultVlan := kubeovnv1.Vlan{
		ObjectMeta: metav1.ObjectMeta{Name: c.config.DefaultVlanName},
		Spec: kubeovnv1.VlanSpec{
			ID:       c.config.DefaultVlanID,
			Provider: c.config.DefaultProviderName,
		},
	}

	_, err = c.config.KubeOvnClient.KubeovnV1().Vlans().Create(context.Background(), &defaultVlan, metav1.CreateOptions{})
	return err
}

func (c *Controller) initSyncCrdIPs() error {
	klog.Info("start to sync ips")
	ips, err := c.config.KubeOvnClient.KubeovnV1().IPs().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	for _, ipCr := range ips.Items {
		ip := ipCr.DeepCopy()
		v4IP, v6IP := util.SplitStringIP(ip.Spec.IPAddress)
		if ip.Spec.V4IPAddress == v4IP && ip.Spec.V6IPAddress == v6IP {
			continue
		}
		ip.Spec.V4IPAddress = v4IP
		ip.Spec.V6IPAddress = v6IP

		_, err := c.config.KubeOvnClient.KubeovnV1().IPs().Update(context.Background(), ip, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("failed to sync crd ip %s: %v", ip.Spec.IPAddress, err)
			return err
		}
	}
	return nil
}

func (c *Controller) initSyncCrdSubnets() error {
	klog.Info("start to sync subnets")
	subnets, err := c.subnetsLister.List(labels.Everything())
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	for _, orisubnet := range subnets {
		subnet := orisubnet.DeepCopy()
		if util.CheckProtocol(subnet.Spec.CIDRBlock) == kubeovnv1.ProtocolDual {
			err = calcDualSubnetStatusIP(subnet, c)
		} else {
			err = calcSubnetStatusIP(subnet, c)
		}
		if err != nil {
			klog.Errorf("failed to calculate subnet %s used ip: %v", subnet.Name, err)
			return err
		}
	}
	return nil
}

func (c *Controller) initSyncCrdVlans() error {
	klog.Info("start to sync vlans")
	vlans, err := c.vlansLister.List(labels.Everything())
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	for _, vlan := range vlans {
		var needUpdate bool
		newVlan := vlan.DeepCopy()
		if newVlan.Spec.VlanId != 0 && newVlan.Spec.ID == 0 {
			newVlan.Spec.ID = newVlan.Spec.VlanId
			newVlan.Spec.VlanId = 0
			needUpdate = true
		}
		if newVlan.Spec.ProviderInterfaceName != "" && newVlan.Spec.Provider == "" {
			newVlan.Spec.Provider = newVlan.Spec.ProviderInterfaceName
			newVlan.Spec.ProviderInterfaceName = ""
			needUpdate = true
		}
		if needUpdate {
			if _, err = c.config.KubeOvnClient.KubeovnV1().Vlans().Update(context.Background(), newVlan, metav1.UpdateOptions{}); err != nil {
				klog.Errorf("failed to update spec of vlan %s: %v", newVlan.Name, err)
				return err
			}
		}
	}

	return nil
}

func (c *Controller) migrateNodeRoute(af int, node, ip, nexthop string, cidrs []string) error {
	if err := c.ovnClient.DeleteLogicalRouterStaticRoute(c.config.ClusterRouter, nil, ip, ""); err != nil {
		klog.Errorf("failed to delete obsolete static route for node %s: %v", node, err)
		return err
	}

	asName := nodeUnderlayAddressSetName(node, af)
	if err := c.ovnClient.CreateAddressSet(asName, nil); err != nil {
		klog.Errorf("failed to create address set %s for node %s: %v", asName, node, err)
		return err
	}
	if err := c.ovnClient.AddressSetUpdateAddress(asName, cidrs...); err != nil {
		klog.Errorf("set ips to address set %s: %v", asName, err)
		return err
	}

	match := fmt.Sprintf("ip%d.dst == %s && ip%d.src != $%s", af, ip, af, asName)
	if err := c.ovnClient.AddLogicalRouterPolicy(c.config.ClusterRouter, util.NodeRouterPolicyPriority, match, "reroute", nexthop, nil); err != nil {
		klog.Errorf("failed to add logical router policy for node %s: %v", node, err)
		return err
	}

	return nil
}

func (c *Controller) initNodeRoutes() error {
	subnets, err := c.subnetsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list subnets: %v", err)
		return err
	}
	nodes, err := c.nodesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list nodes: %v", err)
		return err
	}
	for _, node := range nodes {
		nodeIPv4, nodeIPv6 := util.GetNodeInternalIP(*node)

		var v4CIDRs, v6CIDRs []string
		for _, subnet := range subnets {
			if subnet.Spec.Vlan == "" || !subnet.Spec.LogicalGateway || subnet.Spec.Vpc != util.DefaultVpc {
				continue
			}

			v4, v6 := util.SplitStringIP(subnet.Spec.CIDRBlock)
			if util.CIDRContainIP(v4, nodeIPv4) {
				v4CIDRs = append(v4CIDRs, v4)
			}
			if util.CIDRContainIP(v6, nodeIPv6) {
				v6CIDRs = append(v6CIDRs, v6)
			}
		}

		joinAddrV4, joinAddrV6 := util.SplitStringIP(node.Annotations[util.IpAddressAnnotation])
		if nodeIPv4 != "" && joinAddrV4 != "" {
			if err = c.migrateNodeRoute(4, node.Name, nodeIPv4, joinAddrV4, v4CIDRs); err != nil {
				klog.Errorf("failed to migrate IPv4 route for node %s: %v", node.Name, err)
			}
		}
		if nodeIPv6 != "" && joinAddrV6 != "" {
			if err = c.migrateNodeRoute(6, node.Name, nodeIPv6, joinAddrV6, v6CIDRs); err != nil {
				klog.Errorf("failed to migrate IPv6 route for node %s: %v", node.Name, err)
			}
		}
	}

	return nil
}

func (c *Controller) initAppendLspExternalIds(portName string, externalIDs map[string]string) error {
	if err := c.ovnClient.SetLogicalSwitchPortExternalIds(portName, externalIDs); err != nil {
		klog.Errorf("set lsp external_ids for logical switch port %s: %v", portName, err)
		return err
	}
	return nil
}

// InitHtbQos init high/medium/low qos crd
func (c *Controller) initHtbQos() error {
	var err error
	qosNames := []string{util.HtbQosHigh, util.HtbQosMedium, util.HtbQosLow}
	var priority string

	for _, qosName := range qosNames {
		_, err = c.config.KubeOvnClient.KubeovnV1().HtbQoses().Get(context.Background(), qosName, metav1.GetOptions{})
		if err == nil {
			continue
		}

		if !k8serrors.IsNotFound(err) {
			klog.Errorf("failed to get default htb qos %s: %v", qosName, err)
			continue
		}

		switch qosName {
		case util.HtbQosHigh:
			priority = "100"
		case util.HtbQosMedium:
			priority = "200"
		case util.HtbQosLow:
			priority = "300"
		default:
			klog.Errorf("qos %s is not default defined", qosName)
		}

		htbQos := kubeovnv1.HtbQos{
			TypeMeta:   metav1.TypeMeta{Kind: "HTBQOS"},
			ObjectMeta: metav1.ObjectMeta{Name: qosName},
			Spec: kubeovnv1.HtbQosSpec{
				Priority: priority,
			},
		}

		if _, err = c.config.KubeOvnClient.KubeovnV1().HtbQoses().Create(context.Background(), &htbQos, metav1.CreateOptions{}); err != nil {
			klog.Errorf("create htb qos %s failed: %v", qosName, err)
			continue
		}
	}
	return err
}

func (c *Controller) initNodeChassis() error {
	nodes, err := c.nodesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list nodes: %v", err)
		return err
	}

	for _, node := range nodes {
		chassisName := node.Annotations[util.ChassisAnnotation]
		if chassisName != "" {
			exist, err := c.ovnLegacyClient.ChassisExist(chassisName)
			if err != nil {
				klog.Errorf("failed to check chassis exist: %v", err)
				return err
			}
			if exist {
				err = c.ovnLegacyClient.InitChassisNodeTag(chassisName, node.Name)
				if err != nil {
					klog.Errorf("failed to set chassis nodeTag: %v", err)
					return err
				}
			}
		}
	}
	return nil
}
