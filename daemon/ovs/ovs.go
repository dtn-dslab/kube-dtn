package ovs

import (
	"fmt"
	oovs "github.com/digitalocean/go-openvswitch/ovs"
	"strconv"
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
