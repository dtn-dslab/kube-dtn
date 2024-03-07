package common

import (
	"fmt"
	"github.com/digitalocean/go-openvswitch/ovs"
	koko "github.com/redhat-nfvpe/koko/api"
	"github.com/vishvananda/netlink"
	"os/exec"
	"strconv"
	"strings"
)

const (
	HostBridge         = "ovs-br-host"
	DPUBridge          = "ovs-br-dpu"
	ToHostPort         = "patch-to-host"
	ToDPUPort          = "patch-to-dpu"
	VxlanOutPortPrefix = "vxlan-out"
	VethPodSideSuffix  = "-inner"
	ALL_ONE_MAC        = "ff:ff:ff:ff:ff:ff"
	ALL_ZERO_MAC       = "00:00:00:00:00:00"
)

// For simplicity, generate port name by node IP
func GetVxlanOutPortName(remoteNodeIp string) string {
	return VxlanOutPortPrefix + "-" + strconv.Itoa(int(Hash(remoteNodeIp)))
}

func PrintOVSInfo() (string, error) {
	// sudo ovs-vsctl show
	cmd := exec.Command("ovs-vsctl", "show")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to print ovs info: %v", err)
	}
	return string(output), nil
}

func GetPortID(bridge, port string) (int, error) {
	// sudo ovs-vsctl get Interface port_name ofport
	cmd := exec.Command("ovs-vsctl", "get", "Interface", port, "ofport")
	output, err := cmd.Output()
	if err != nil {
		return -1, fmt.Errorf("failed to get port %s id on OVS bridge %s: %v", port, bridge, err)
	}
	resultStr := strings.TrimSpace(string(output))
	resultInt, err := strconv.Atoi(resultStr)
	if err != nil {
		return -1, fmt.Errorf("error converting port %s id %s to int: %v", port, resultStr, err)
	}
	return resultInt, nil
}

// name = test-a-1
func ConnectVethToBridge(veth *koko.VEth, c *ovs.Client) error {

	name := veth.LinkName

	// sudo ip l a test-a-1(bridge) type veth peer name test-a-1-inner(pod)
	link_br, link_pod, err := koko.GetVethPair(name, name+VethPodSideSuffix)
	if err != nil {
		return err
	}

	if err = veth.SetVethLink(link_pod); err != nil {
		netlink.LinkDel(link_br)
		netlink.LinkDel(link_pod)
		return err
	}

	// TODO: sudo ip l s test-a-1 up

	// sudo ovs-vsctl add-port br-12 test-a-1
	if err = c.VSwitch.AddPort(HostBridge, name); err != nil {
		netlink.LinkDel(link_br)
		netlink.LinkDel(link_pod)
	}
	return err
}
