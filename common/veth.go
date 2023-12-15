package common

import (
	"context"
	"fmt"
	"net"
	"time"

	koko "github.com/redhat-nfvpe/koko/api"
	"github.com/y-young/kube-dtn/daemon/metrics"
	pb "github.com/y-young/kube-dtn/proto/v1"
)

// Creates koko.Veth from NetNS and LinkName
func MakeVeth(ctx context.Context, netNS, linkName, ip, mac string) (*koko.VEth, error) {
	logger := GetLogger(ctx)

	logger.Infof("Creating Veth struct with NetNS: %s and intfName: %s, IP: %s, MAC: %s", netNS, linkName, ip, mac)
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
func CreateVeth(ctx context.Context, self *koko.VEth, peer *koko.VEth, properties *pb.LinkProperties, m *metrics.LatencyHistograms) error {
	err := koko.MakeVeth(*self, *peer)
	if err != nil {
		return err
	}

	start := time.Now()
	qdiscs, err := MakeQdiscs(ctx, properties)
	if err != nil {
		return fmt.Errorf("failed to construct qdiscs: %s", err)
	}
	m.Observe("VethMakeQdiscs", time.Since(start).Milliseconds())

	start = time.Now()
	err = SetVethQdiscs(ctx, self, qdiscs)
	if err != nil {
		return fmt.Errorf("failed to set qdiscs on self veth %s: %v", self, err)
	}
	m.Observe("VethSetVethQdiscs", time.Since(start).Milliseconds())

	start = time.Now()
	err = SetVethQdiscs(ctx, peer, qdiscs)
	if err != nil {
		return fmt.Errorf("failed to set qdiscs on peer veth %s: %v", self, err)
	}
	m.Observe("VethSetVethQdiscs", time.Since(start).Milliseconds())
	return nil
}

// Setup a veth pair, if the interfaces already exist, do nothing, remove the stale interfaces if only one exists
func SetupVeth(ctx context.Context, self *koko.VEth, peer *koko.VEth, link *pb.Link, localPod *pb.Pod, m *metrics.LatencyHistograms, detect bool) (err error) {
	logger := GetLogger(ctx)

	start := time.Now()
	if detect {
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
	}
	m.Observe("VXLANDetect", time.Since(start).Milliseconds())

	return CreateVeth(ctx, self, peer, link.Properties, m)
}
