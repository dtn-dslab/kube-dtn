package vxlan

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	koko "github.com/redhat-nfvpe/koko/api"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"github.com/y-young/kube-dtn/common"
	"github.com/y-young/kube-dtn/daemon/metrics"
	pb "github.com/y-young/kube-dtn/proto/v1"
)

type VxlanSpec struct {
	NetNs    string
	IntfName string
	IntfIp   string
	PeerVtep string
	Vni      int32
	// IP of VXLAN source interface
	SrcIp string
	// Name of VXLAN source interface
	SrcIntf string
}

// Set up VXLAN link with qdiscs
func SetupVxLan(ctx context.Context, v *VxlanSpec, properties *pb.LinkProperties, m *metrics.LatencyHistograms, detect bool) (err error) {
	logger := common.GetLogger(ctx)

	start := time.Now()
	var veth *koko.VEth
	if veth, err = CreateOrUpdate(ctx, v, m, detect); err != nil {
		logger.Errorf("Failed to setup VXLAN: %v", err)
		return err
	}
	m.Observe("VXLANCreateOrUpdate", time.Since(start).Milliseconds())

	start = time.Now()
	qdiscs, err := common.MakeQdiscs(ctx, properties)
	if err != nil {
		logger.Errorf("Failed to construct qdiscs: %v", err)
		return err
	}
	m.Observe("VXLANMakeQdiscs", time.Since(start).Milliseconds())

	start = time.Now()
	err = common.SetVethQdiscs(ctx, veth, qdiscs)
	if err != nil {
		logger.Errorf("Failed to set qdisc on self veth %s: %v", veth, err)
		return err
	}
	m.Observe("VXLANSetVethQdiscs", time.Since(start).Milliseconds())

	return nil
}

// CreateOrUpdate creates or updates the vxlan on the node.
func CreateOrUpdate(ctx context.Context, v *VxlanSpec, m *metrics.LatencyHistograms, detect bool) (*koko.VEth, error) {
	logger := common.GetLogger(ctx)

	var err error
	// Looking up VXLAN source interface
	srcIP := v.SrcIp
	srcIntf := v.SrcIntf
	if srcIntf == "" {
		if srcIP == "" {
			srcIP, srcIntf, err = GetDefaultVxlanSource()
		} else {
			start := time.Now()
			srcIP, srcIntf, err = GetVxlanSource(srcIP)
			m.Observe("GetVxlanSource", time.Since(start).Milliseconds())
		}
		if err != nil {
			return nil, err
		}
	}
	logger.Infof("VXLAN route is via %s@%s", srcIP, srcIntf)

	// Creating koko Veth struct
	veth := koko.VEth{
		NsName:   v.NetNs,
		LinkName: v.IntfName,
	}

	// Link IP is optional, only set it when it's provided
	if v.IntfIp != "" {
		ipAddr, ipSubnet, err := net.ParseCIDR(v.IntfIp)
		if err != nil {
			return nil, fmt.Errorf("error parsing CIDR %s: %s", v.IntfIp, err)
		}
		veth.IPAddr = []net.IPNet{{
			IP:   ipAddr,
			Mask: ipSubnet.Mask,
		}}
	}
	logger.Infof("Created koko Veth struct %+v", veth)

	// Creating koko vxlan struct
	vxlan := koko.VxLan{
		ParentIF: srcIntf,
		IPAddr:   net.ParseIP(v.PeerVtep),
		ID:       int(v.Vni),
	}
	logger.Infof("Created koko vxlan struct %+v", vxlan)

	// Try to read interface attributes from netlink
	// link := getLinkFromNS(ctx, veth.NsName, veth.LinkName)
	// logger.Infof("Retrieved %s link from %s Netns: %+v", veth.LinkName, veth.NsName, link)

	// Check if interface already exists
	// vxlanLink, isVxlan := link.(*netlink.Vxlan)
	// logger.Infof("Is link %s a VXLAN?: %s", veth.LinkName, strconv.FormatBool(isVxlan))
	// if isVxlan { // the link we've found is a vxlan link
	// 	if vxlanEqual(vxlanLink, vxlan) { // If Vxlan attrs are the same, nothing to do
	// 		logger.Infof("Vxlan attrs are the same")
	// 		return &veth, nil
	// 	}

	// 	// If Vxlan attrs are different
	// 	// We remove the existing link and add a new one
	// 	logger.Infof("Vxlan attrs are different: %d!=%d or %v!=%v", vxlanLink.VxlanId, vxlan.ID, vxlanLink.Group, vxlan.IPAddr)
	// 	if err = veth.RemoveVethLink(); err != nil {
	// 		return nil, fmt.Errorf("error when removing an old Vxlan interface with koko: %s", err)
	// 	}
	// } else { // the link we've found isn't a vxlan or doesn't exist
	// 	logger.Infof("Link %+v we've found isn't a vxlan or doesn't exist", link)
	// 	// If link exists but wasn't matched as vxlan, we need to delete it
	// 	if link != nil {
	// 		logger.Infof("Attempting to remove link %+v", veth)
	// 		if err = veth.RemoveVethLink(); err != nil {
	// 			return nil, fmt.Errorf("error when removing an old non-Vxlan interface with koko: %s", err)
	// 		}
	// 	}
	// }

	logger.Infof("Creating a VXLAN link: %v; inside pod: %v", vxlan, veth)

	if detect {
		if e := veth.RemoveVethLink(); e != nil {
			logger.Infof("Failed to remove an old interface with koko: %s", e)
		}
	}

	start := time.Now()
	err = koko.MakeVxLan(veth, vxlan)
	m.Observe("MakeVxLan", time.Since(start).Milliseconds())

	// if err != nil {
	// 	logger.Infof("Error when creating a Vxlan interface with koko: %s", err)
	// 	if strings.Contains(err.Error(), "file exists") {
	// 		logger.Infof("Trying to remove conflicting link with VNI %d", vxlan.ID)
	// 		// Conflicting interface name or VNI will incur this error,
	// 		// since we've removed the old interface previously,
	// 		// it's likely that another link with the same VNI already exists, remove it and try again.
	// 		start := time.Now()
	// 		if e := veth.RemoveVethLink(); e != nil {
	// 			logger.Errorf("Failed to remove an old interface with koko: %s", err)
	// 		}
	// 		if e := RemoveLinkWithVni(ctx, v.Vni, veth.NsName); e != nil {
	// 			logger.Errorf("Failed to remove conflicting link with VNI %d: %s", vxlan.ID, err)
	// 		}
	// 		m.Observe("RemoveLinkWhenExists", time.Since(start).Milliseconds())

	// 		start = time.Now()
	// 		err = koko.MakeVxLan(veth, vxlan)
	// 		m.Observe("MakeVxLanAgain", time.Since(start).Milliseconds())
	// 	}
	// }

	if err != nil {
		return nil, fmt.Errorf("error when creating a Vxlan interface with koko: %s", err)
	}

	return &veth, nil
}

// getLinkFromNS retrieves netlink.Link from NetNS
func getLinkFromNS(ctx context.Context, nsName string, linkName string) netlink.Link {
	logger := common.GetLogger(ctx)

	// If namespace doesn't exist, do nothing and return empty result
	vethNs, err := ns.GetNS(nsName)
	if err != nil {
		return nil
	}
	defer vethNs.Close()
	// We can ignore the error returned here as we will create that interface instead
	var result netlink.Link
	err = vethNs.Do(func(_ ns.NetNS) error {
		var err error
		result, err = netlink.LinkByName(linkName)
		return err
	})
	if err != nil {
		logger.Infof("Link %s doesn't exist in %s", linkName, nsName)
	}

	return result
}

// Uses netlink to get the iface reliably given an IP address.
func GetVxlanSource(nodeIP string) (srcIP string, srcIntf string, err error) {
	if nodeIP == "" {
		return "", "", fmt.Errorf("kubedtnd provided no HOST_IP address: %s", nodeIP)
	}
	nIP := net.ParseIP(nodeIP)
	if nIP == nil {
		return "", "", fmt.Errorf("parsing failed for kubedtnd provided no HOST_IP address: %s", nodeIP)
	}
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if nIP.Equal(ip) {
				log.Infof("Found iface %s for address %s", i.Name, nodeIP)
				return nodeIP, i.Name, nil
			}
		}
	}
	return "", "", fmt.Errorf("no iface found for address %s", nodeIP)
}

// Uses netlink to query the IP and LinkName of the interface with default route
func GetDefaultVxlanSource() (srcIP string, srcIntf string, err error) {
	// Looking up a default route to get the intf and IP for vxlan
	r, err := netlink.RouteGet(net.IPv4(1, 1, 1, 1))
	if (err != nil) && len(r) < 1 {
		return "", "", fmt.Errorf("error getting default route: %s\n%+v", err, r)
	}
	srcIP = r[0].Src.String()

	link, err := netlink.LinkByIndex(r[0].LinkIndex)
	if err != nil {
		return "", "", fmt.Errorf("error looking up link by its index: %s", err)
	}
	srcIntf = link.Attrs().Name
	return srcIP, srcIntf, nil
}

func vxlanEqual(l1 *netlink.Vxlan, l2 koko.VxLan) bool {
	if l1.VxlanId != l2.ID {
		return false
	}
	if !l1.Group.Equal(l2.IPAddr) {
		return false
	}
	return true
}

// RemoveLinkWithVni removes a vxlan link with the given VNI
func RemoveLinkWithVni(ctx context.Context, vni int32, netns string) error {
	logger := common.GetLogger(ctx)

	vethNs, err := ns.GetNS(netns)
	if err != nil {
		return err
	}
	defer vethNs.Close()

	return vethNs.Do(func(_ ns.NetNS) error {
		links, err := netlink.LinkList()
		if err != nil {
			return fmt.Errorf("error listing links: %s", err)
		}

		for _, link := range links {
			vxlanLink, isVxlan := link.(*netlink.Vxlan)
			if !isVxlan {
				continue
			}
			if int32(vxlanLink.VxlanId) == vni {
				if err = netlink.LinkDel(vxlanLink); err != nil {
					return fmt.Errorf("error deleting vxlan link: %s", err)
				}
				logger.Infof("Successfully removed vxlan link %+v", vxlanLink)
				return nil
			}
		}

		logger.Infof("No link with vni %d found", vni)
		return nil
	})
}
