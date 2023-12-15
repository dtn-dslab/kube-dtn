package kubedtn

import (
	"context"
	"fmt"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	koko "github.com/redhat-nfvpe/koko/api"
	"github.com/vishvananda/netlink"
	"github.com/y-young/kube-dtn/daemon/metrics"
)

func RemoveVethLink(ctx context.Context, veth *koko.VEth, m *metrics.LatencyHistograms) (err error) {
	// logger := common.GetLogger(ctx).WithFields(log.Fields{
	// 	"link": veth.LinkName,
	// })

	var vethNs ns.NetNS
	var link netlink.Link

	start := time.Now()
	if veth.NsName == "" {
		if vethNs, err = ns.GetCurrentNS(); err != nil {
			return fmt.Errorf("%v", err)
		}
	} else {
		if vethNs, err = ns.GetNS(veth.NsName); err != nil {
			return fmt.Errorf("%v", err)
		}
	}
	defer vethNs.Close()
	elapsed := time.Since(start)
	// logger.Infof("Dellink: GetNS took %s", elapsed)
	m.Observe("RemoveGetNS", elapsed.Milliseconds())

	all_start := time.Now()
	err = vethNs.Do(func(_ ns.NetNS) error {
		start = time.Now()
		if veth.MirrorIngress != "" {
			if err = veth.UnsetIngressMirror(); err != nil {
				return fmt.Errorf(
					"failed to unset tc ingress mirror :%v",
					err)
			}
		}
		if veth.MirrorEgress != "" {
			if err = veth.UnsetEgressMirror(); err != nil {
				return fmt.Errorf(
					"failed to unset tc egress mirror: %v",
					err)
			}
		}
		elapsed = time.Since(start)
		// logger.Infof("Dellink: UnsetMirror took %s", elapsed)
		m.Observe("RemoveUnsetMirror", elapsed.Milliseconds())

		start = time.Now()
		if link, err = netlink.LinkByName(veth.LinkName); err != nil {
			return fmt.Errorf("failed to lookup %q in %q: %v",
				veth.LinkName, vethNs.Path(), err)
		}
		elapsed = time.Since(start)
		// logger.Infof("Dellink: LinkByName took %s", elapsed)
		m.Observe("RemoveLinkByName", elapsed.Milliseconds())

		start = time.Now()
		if err = netlink.LinkDel(link); err != nil {
			return fmt.Errorf("failed to remove link %q in %q: %v",
				veth.LinkName, vethNs.Path(), err)
		}
		elapsed = time.Since(start)
		// logger.Infof("Dellink: LinkDel took %s", elapsed)
		m.Observe("RemoveLinkDel", elapsed.Milliseconds())
		return nil
	})
	elapsed = time.Since(all_start)
	// logger.Infof("Dellink: Do took %s", elapsed)
	m.Observe("RemoveTotal", elapsed.Milliseconds())

	return err
}
