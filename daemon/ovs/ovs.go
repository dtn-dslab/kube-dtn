package ovs

import (
	"fmt"
	"os/exec"
	"strconv"

	oovs "github.com/digitalocean/go-openvswitch/ovs"
)

const (
	patResubmitPortTable = "resubmit(%s,%s)"
)

// Resubmit resubmits a packet for further processing by matching
// flows with the specified port and table.
// Works no matter port and table is compared to original impl.
func Resubmit(port int, table int) oovs.Action {
	return &resubmitAction{
		port:  port,
		table: table,
	}
}

// A resubmitAction is an Action which is used by ConneectionTracking.
type resubmitAction struct {
	port  int
	table int
}

// MarshalText implements Action.
func (a *resubmitAction) MarshalText() ([]byte, error) {

	p := ""
	if a.port != 0 {
		p = strconv.Itoa(a.port)
	}

	t := strconv.Itoa(a.table)

	return bprintf(patResubmitPortTable, p, t), nil
}

// GoString implements Action.
func (a *resubmitAction) GoString() string {
	return fmt.Sprintf("ovs.Resubmit(%d, %d)", a.port, a.table)
}

// bprintf is fmt.Sprintf, but it returns a byte slice instead of a string.
func bprintf(format string, a ...interface{}) []byte {
	return []byte(fmt.Sprintf(format, a...))
}

// Hack to add options:dst_port

// A MyVSwitchService is used in a Client to execute 'ovs-vsctl' commands.
type MyVSwitchService struct {
}

// exec executes an ExecFunc using 'ovs-vsctl'.
func (v *MyVSwitchService) exec(args ...string) error {
	cmd := exec.Command("ovs-vsctl", args...)
	return cmd.Run()
}

// Interface sets configuration for an interface using the values from an
// InterfaceOptions struct.
func (v *MyVSwitchService) SetInterface(ifi string, options InterfaceOptions) error {
	// Prepend command line arguments before expanding options slice
	// and appending it
	args := []string{"set", "interface", ifi}
	args = append(args, options.slice()...)
	return v.exec(args...)
}

// An InterfaceOptions struct enables configuration of an Interface.
type InterfaceOptions struct {
	Type                 oovs.InterfaceType
	Peer                 string
	MTURequest           int
	IngressRatePolicing  int64
	IngressBurstPolicing int64
	RemoteIP             string
	LocalIP              string
	DstPort              int
	Key                  string
}

// slice creates a string slice containing any non-zero option values from the
// struct in the format expected by Open vSwitch.
func (i InterfaceOptions) slice() []string {
	var s []string

	if i.Type != "" {
		s = append(s, fmt.Sprintf("type=%s", i.Type))
	}

	if i.Peer != "" {
		s = append(s, fmt.Sprintf("options:peer=%s", i.Peer))
	}

	if i.MTURequest > 0 {
		s = append(s, fmt.Sprintf("mtu_request=%d", i.MTURequest))
	}

	if i.IngressRatePolicing == oovs.DefaultIngressBurstPolicing {
		// Set to 0 (the default) to disable policing.
		s = append(s, "ingress_policing_rate=0")
	} else if i.IngressRatePolicing > 0 {
		s = append(s, fmt.Sprintf("ingress_policing_rate=%d", i.IngressRatePolicing))
	}

	if i.IngressBurstPolicing == oovs.DefaultIngressBurstPolicing {
		// Set to 0 (the default) to the default burst size.
		s = append(s, "ingress_policing_burst=0")
	} else if i.IngressBurstPolicing > 0 {
		s = append(s, fmt.Sprintf("ingress_policing_burst=%d", i.IngressBurstPolicing))
	}

	if i.RemoteIP != "" {
		s = append(s, fmt.Sprintf("options:remote_ip=%s", i.RemoteIP))
	}

	if i.LocalIP != "" {
		s = append(s, fmt.Sprintf("options:local_ip=%s", i.LocalIP))
	}

	if i.DstPort > 0 {
		s = append(s, fmt.Sprintf("options:dst_port=%d", i.DstPort))
	}

	if i.Key != "" {
		s = append(s, fmt.Sprintf("options:key=%s", i.Key))
	}

	return s
}
