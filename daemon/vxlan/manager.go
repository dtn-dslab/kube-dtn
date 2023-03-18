package vxlan

import (
	"fmt"

	"github.com/containernetworking/plugins/pkg/ns"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	v1 "github.com/y-young/kube-dtn/api/v1"
)

type VxlanManager struct {
	// vni to netns mapping, synchronization is handled by linkMutexes
	vxlans map[int32]*string
}

func NewVxlanManager() *VxlanManager {
	return &VxlanManager{
		vxlans: make(map[int32]*string),
	}
}

func (m *VxlanManager) Init(topologies *v1.TopologyList) {
	for _, topology := range topologies.Items {
		if topology.Status.NetNs == "" {
			continue
		}
		vethNs, err := ns.GetNS(topology.Status.NetNs)
		if err != nil {
			log.Errorf("Failed to get netns %s: %v", topology.Status.NetNs, err)
			continue
		}
		err = vethNs.Do(func(_ ns.NetNS) error {
			links, err := netlink.LinkList()
			if err != nil {
				return fmt.Errorf("error listing links: %s", err)
			}
			for _, link := range links {
				vxlanLink, isVxlan := link.(*netlink.Vxlan)
				if !isVxlan {
					continue
				}
				m.vxlans[int32(vxlanLink.VxlanId)] = &topology.Status.NetNs
				log.Debugf("Found vxlan %d in netns %s", vxlanLink.VxlanId, topology.Status.NetNs)
			}
			return nil
		})
		if err != nil {
			log.Errorf("Error while processing links in %s: %v", topology.Status.NetNs, err)
		}
		vethNs.Close()
	}
}

func (m *VxlanManager) Add(vni int32, netns *string) {
	m.vxlans[vni] = netns
}

func (m *VxlanManager) Delete(vni int32) {
	delete(m.vxlans, vni)
}

func (m *VxlanManager) Get(vni int32) *string {
	return m.vxlans[vni]
}
