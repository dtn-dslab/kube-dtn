package common

import (
	"fmt"
	"net"

	koko "github.com/redhat-nfvpe/koko/api"
	log "github.com/sirupsen/logrus"
	v1 "github.com/y-young/kube-dtn/api/v1"
	pb "github.com/y-young/kube-dtn/proto/v1"
)

// Creates koko.Veth from NetNS and LinkName
func MakeVeth(netNS, linkName, ip, mac string) (*koko.VEth, error) {
	log.Infof("Creating Veth struct with NetNS: %s and intfName: %s, IP: %s, MAC: %s", netNS, linkName, ip, mac)
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

	if mac != "" {
		macAddr, err := net.ParseMAC(mac)
		if err != nil {
			return nil, fmt.Errorf("failed to parse MAC %s: %s", mac, err)
		}
		veth.HardwareAddr = macAddr
	}

	return &veth, nil
}

// Create a veth pair and set qdiscs on both ends
func CreateVeth(self *koko.VEth, peer *koko.VEth, properties *pb.LinkProperties) error {
	err := koko.MakeVeth(*self, *peer)
	if err != nil {
		return err
	}
	qdiscs, err := MakeQdiscs(properties)
	if err != nil {
		return fmt.Errorf("failed to construct qdiscs: %s", err)
	}
	err = SetVethQdiscs(self, qdiscs)
	if err != nil {
		return fmt.Errorf("failed to set qdiscs on self veth %s: %v", self, err)
	}
	err = SetVethQdiscs(peer, qdiscs)
	if err != nil {
		return fmt.Errorf("failed to set qdiscs on peer veth %s: %v", self, err)
	}
	return nil
}

// Setup a veth pair, if the interfaces already exist, do nothing, remove the stale interfaces if only one exists
func SetupVeth(logger *log.Entry, self *koko.VEth, peer *koko.VEth, link *pb.Link, localPod *pb.Pod, peerTopology *v1.Topology) (err error) {
	// Checking if interfaces already exist
	iExist, _ := koko.IsExistLinkInNS(self.NsName, self.LinkName)
	pExist, _ := koko.IsExistLinkInNS(peer.NsName, peer.LinkName)
	logger.Infof("Does the link already exist? Local: %t, Peer: %t", iExist, pExist)

	if iExist && pExist { // If both link exist, we don't need to do anything
		logger.Info("Both interfaces already exist in namespace")
		return nil
	}

	if !iExist && pExist { // If only peer link exists, we need to destroy it first
		logger.Info("Only peer link exists, removing it first")
		if err := peer.RemoveVethLink(); err != nil {
			logger.Infof("Failed to remove a stale interface %s of peer %s", peer.LinkName, link.PeerPod)
			return err
		}
	} else if iExist && !pExist { // If only local link exists, we need to destroy it first
		logger.Infof("Only local link exists, removing it first")
		if err := self.RemoveVethLink(); err != nil {
			logger.Infof("Failed to remove a local stale interface %s for pod %s", self.LinkName, localPod.Name)
			return err
		}
	}

	return CreateVeth(self, peer, link.Properties)
}
