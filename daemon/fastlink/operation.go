package kubedtn

import (
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
)

func RemoveVethLink(nsName string, linkName string) error {
	var vethNS ns.NetNS
	var link netlink.Link
	link.Attrs().Name = linkName
	vethNS, err := ns.GetNS(nsName)
	if err != nil {
		return err
	}

	defer vethNS.Close()

	err = vethNS.Do(func(_ ns.NetNS) error {
		if err = netlink.LinkDel(link); err != nil {
			return err
		}
		return nil
	})

	return err
}
