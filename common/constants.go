package common

import (
	"github.com/vishvananda/netlink"
)

const (
	VxlanBase             = 5000
	DefaultPort           = "51111"
	Localhost             = "localhost"
	LocalDaemon           = Localhost + ":" + DefaultPort
	MacvlanMode           = netlink.MACVLAN_MODE_BRIDGE
	INTER_NODE_LINK_VXLAN = "VXLAN"
	INTER_NODE_LINK_GRPC  = "GRPC"
)