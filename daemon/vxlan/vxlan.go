package vxlan

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/redhat-nfvpe/koko/api"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

type VxlanSpec struct {
	NetNs    string
	IntfName string
	IntfIp   string
	PeerVtep string
	Vni      int32
	// IP of VXLAN source interface
	SrcIp string
}

var vxlanLogger *log.Entry = nil

func InitLogger() {
	vxlanLogger = log.WithFields(log.Fields{"daemon": "kubedtnd", "overlay": "vxLAN"})
}

// CreateOrUpdate creates or updates the vxlan on the node.
func CreateOrUpdate(v *VxlanSpec) (*api.VEth, error) {
	var err error
	// Looking up VXLAN source interface
	var srcIP string
	var srcIntf string
	if v.SrcIp == "" {
		srcIP, srcIntf, err = GetDefaultVxlanSource()
	} else {
		srcIP, srcIntf, err = GetVxlanSource(v.SrcIp)
	}
	if err != nil {
		return nil, err
	}
	vxlanLogger.Infof("VXLAN route is via %s@%s", srcIP, srcIntf)

	// Creating koko Veth struct
	veth := api.VEth{
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
	vxlanLogger.Infof("Created koko Veth struct %+v", veth)

	// Creating koko vxlan struct
	vxlan := api.VxLan{
		ParentIF: srcIntf,
		IPAddr:   net.ParseIP(v.PeerVtep),
		ID:       int(v.Vni),
	}
	vxlanLogger.Infof("Created koko vxlan struct %+v", vxlan)

	// Try to read interface attributes from netlink
	link := getLinkFromNS(veth.NsName, veth.LinkName)
	vxlanLogger.Infof("Retrieved %s link from %s Netns: %+v", veth.LinkName, veth.NsName, link)

	// Check if interface already exists
	vxlanLink, isVxlan := link.(*netlink.Vxlan)
	vxlanLogger.Infof("Is link %s a VXLAN?: %s", veth.LinkName, strconv.FormatBool(isVxlan))
	if isVxlan { // the link we've found is a vxlan link
		if vxlanEqual(vxlanLink, vxlan) { // If Vxlan attrs are the same, nothing to do
			vxlanLogger.Infof("Vxlan attrs are the same")
			return &veth, nil
		}

		// If Vxlan attrs are different
		// We remove the existing link and add a new one
		vxlanLogger.Infof("Vxlan attrs are different: %d!=%d or %v!=%v", vxlanLink.VxlanId, vxlan.ID, vxlanLink.Group, vxlan.IPAddr)
		if err = veth.RemoveVethLink(); err != nil {
			return nil, fmt.Errorf("error when removing an old Vxlan interface with koko: %s", err)
		}
	} else { // the link we've found isn't a vxlan or doesn't exist
		vxlanLogger.Infof("Link %+v we've found isn't a vxlan or doesn't exist", link)
		// If link exists but wasn't matched as vxlan, we need to delete it
		if link != nil {
			vxlanLogger.Infof("Attempting to remove link %+v", veth)
			if err = veth.RemoveVethLink(); err != nil {
				return nil, fmt.Errorf("error when removing an old non-Vxlan interface with koko: %s", err)
			}
		}
	}

	vxlanLogger.Infof("Creating a VXLAN link: %v; inside pod: %v", vxlan, veth)
	if err = api.MakeVxLan(veth, vxlan); err != nil {
		vxlanLogger.Infof("Error when creating a Vxlan interface with koko: %s", err)
		if strings.Contains(err.Error(), "file exists") {
			vxlanLogger.Infof("Trying to remove conflicting link with VNI %d", vxlan.ID)
			// Conflicting interface name or VNI will incur this error,
			// since we've removed the old interface previously,
			// it's likely that another link with the same VNI already exists, remove it and try again.
			if e := RemoveLinkWithVni(vxlan.ID, veth.NsName); e != nil {
				vxlanLogger.Errorf("Failed to remove conflicting link with VNI %d: %s", vxlan.ID, err)
				return nil, err
			}
			err = api.MakeVxLan(veth, vxlan)
		}
	}

	if err != nil {
		return nil, fmt.Errorf("error when creating a Vxlan interface with koko: %s", err)
	}

	return &veth, nil
}

// getLinkFromNS retrieves netlink.Link from NetNS
func getLinkFromNS(nsName string, linkName string) netlink.Link {
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
		vxlanLogger.Infof("Failed to get link: %s", linkName)
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

func vxlanEqual(l1 *netlink.Vxlan, l2 api.VxLan) bool {
	if l1.VxlanId != l2.ID {
		return false
	}
	if !l1.Group.Equal(l2.IPAddr) {
		return false
	}
	return true
}

// RemoveLinkWithVni removes a vxlan link with the given VNI
func RemoveLinkWithVni(vni int, netns string) error {
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
			if vxlanLink.VxlanId == vni {
				if err = netlink.LinkDel(vxlanLink); err != nil {
					return fmt.Errorf("error deleting vxlan link: %s", err)
				}
				vxlanLogger.Infof("Successfully removed vxlan link %+v", vxlanLink)
				return nil
			}
		}

		vxlanLogger.Infof("No link with vni %d found", vni)
		return nil
	})
}
