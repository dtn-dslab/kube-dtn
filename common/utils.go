package common

import (
	"context"
	"fmt"
	"net"

	koko "github.com/redhat-nfvpe/koko/api"
	log "github.com/sirupsen/logrus"
	pb "github.com/y-young/kube-dtn/proto/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Uses netlink to get the iface reliably given an IP address.
func GetVxlanSource(nodeIP string) (string, string, error) {
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

// Creates koko.Veth from NetNS and LinkName
func MakeVeth(netNS, linkName string, ip string) (*koko.VEth, error) {
	log.Infof("Creating Veth struct with NetNS: %s and intfName: %s, IP: %s", netNS, linkName, ip)
	veth := koko.VEth{}
	veth.NsName = netNS
	veth.LinkName = linkName
	if ip != "" {
		ipAddr, ipSubnet, err := net.ParseCIDR(ip)
		if err != nil {
			return nil, fmt.Errorf("failed to parse CIDR %s: %s", ip, err)
		}
		veth.IPAddr = []net.IPNet{{
			IP:   ipAddr,
			Mask: ipSubnet.Mask,
		}}
	}
	return &veth, nil
}

// Creates koko.Vxlan from ParentIF, destination IP and VNI
func MakeVxlan(srcIntf string, peerIP string, idx int64) *koko.VxLan {
	return &koko.VxLan{
		ParentIF: srcIntf,
		IPAddr:   net.ParseIP(peerIP),
		ID:       int(VxlanBase + idx),
	}
}

// Call remote daemon to set up link on their side
func UpdateRemote(ctx context.Context, localPod *pb.Pod, peerPod *pb.Pod, link *pb.Link) error {
	payload := &pb.RemotePod{
		NetNs:      peerPod.NetNs,
		IntfName:   link.PeerIntf,
		IntfIp:     link.PeerIp,
		PeerVtep:   localPod.SrcIp,
		Vni:        link.Uid + VxlanBase,
		KubeNs:     localPod.KubeNs,
		Properties: link.Properties,
	}

	url := fmt.Sprintf("%s:%s", peerPod.SrcIp, DefaultPort)
	log.Infof("Trying to do a remote update on %s", url)

	remote, err := grpc.Dial(url, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Infof("Failed to dial remote gRPC url %s", url)
		return err
	}
	remoteClient := pb.NewRemoteClient(remote)
	ok, err := remoteClient.Update(ctx, payload)
	if err != nil || !ok.Response {
		log.Infof("Failed to do a remote update: %s", err)
		return err
	}
	return nil
}

// Set up VXLAN link with qdiscs
func MakeVxLan(self *koko.VEth, vxlan *koko.VxLan, link *pb.Link) error {
	err := koko.MakeVxLan(*self, *vxlan)
	if err != nil {
		return err
	}
	qdiscs, err := MakeQdiscs(link.Properties)
	if err != nil {
		log.Errorf("Failed to construct qdiscs: %s", err)
		return err
	}
	err = SetVethQdiscs(self, qdiscs)
	if err != nil {
		log.Errorf("Failed to set qdisc on self veth %s: %v", self, err)
		return err
	}
	return nil
}
