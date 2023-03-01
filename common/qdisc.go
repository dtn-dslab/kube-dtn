package common

import (
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	koko "github.com/redhat-nfvpe/koko/api"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	pb "github.com/y-young/kube-dtn/proto/v1"
)

func MakeQdiscs(properties *pb.LinkProperties) ([]netlink.Qdisc, error) {
	qdiscs := []netlink.Qdisc{}
	if properties == nil {
		return qdiscs, nil
	}

	latency, err := ParseDuration(properties.Latency)
	if err != nil {
		log.Errorf("Invalid latency value: %s", err)
		return nil, err
	}

	latencyCorr, err := ParseFloatPercentage(properties.LatencyCorr)
	if err != nil {
		log.Errorf("Invalid latency correlation value: %s", err)
		return nil, err
	}

	jitter, err := ParseDuration(properties.Jitter)
	if err != nil {
		log.Errorf("Invalid jitter value: %s", err)
		return nil, err
	}

	loss, err := ParseFloatPercentage(properties.Loss)
	if err != nil {
		log.Errorf("Invalid loss value: %s", err)
		return nil, err
	}

	lossCorr, err := ParseFloatPercentage(properties.LossCorr)
	if err != nil {
		log.Errorf("Invalid loss correlation value: %s", err)
		return nil, err
	}

	duplicate, err := ParseFloatPercentage(properties.Duplicate)
	if err != nil {
		log.Errorf("Invalid duplicate value: %s", err)
		return nil, err
	}

	duplicateCorr, err := ParseFloatPercentage(properties.DuplicateCorr)
	if err != nil {
		log.Errorf("Invalid duplicate correlation value: %s", err)
		return nil, err
	}

	reorderProb, err := ParseFloatPercentage(properties.ReorderProb)
	if err != nil {
		log.Errorf("Invalid reorder probability value: %s", err)
		return nil, err
	}

	reorderCorr, err := ParseFloatPercentage(properties.ReorderCorr)
	if err != nil {
		log.Errorf("Invalid reorder correlation value: %s", err)
		return nil, err
	}

	corruptProb, err := ParseFloatPercentage(properties.CorruptProb)
	if err != nil {
		log.Errorf("Invalid corrupt probability value: %s", err)
		return nil, err
	}

	corruptCorr, err := ParseFloatPercentage(properties.CorruptCorr)
	if err != nil {
		log.Errorf("Invalid corrupt correlation value: %s", err)
		return nil, err
	}

	netemQdisc := netlink.NewNetem(netlink.QdiscAttrs{}, netlink.NetemQdiscAttrs{
		Latency:       latency,
		DelayCorr:     latencyCorr,
		Jitter:        jitter,
		Loss:          loss,
		LossCorr:      lossCorr,
		Gap:           properties.Gap,
		Duplicate:     duplicate,
		DuplicateCorr: duplicateCorr,
		ReorderProb:   reorderProb,
		ReorderCorr:   reorderCorr,
		CorruptProb:   corruptProb,
		CorruptCorr:   corruptCorr,
	})
	qdiscs = append(qdiscs, netemQdisc)

	rate, err := ParseRate(properties.Rate)
	if err != nil {
		log.Errorf("Invalid rate value: %s", err)
		return nil, err
	}
	if rate != 0 {
		tbfQdisc := &netlink.Tbf{
			Rate:   rate,
			Buffer: getTbfBurst(rate),
			// Limit will be set through latency in command
			Minburst: 1500,
		}
		qdiscs = append(qdiscs, tbfQdisc)
	}

	return qdiscs, nil
}

func ParseFloatPercentage(str string) (float32, error) {
	if str == "" {
		return 0, nil
	}
	value, err := strconv.ParseFloat(str, 32)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(value) {
		return 0, fmt.Errorf("percentage value must be a number")
	}
	if value < 0 || value > 100 {
		return 0, fmt.Errorf("percentage value must be between 0 and 100")
	}
	return float32(value), nil
}

// Parse a duration string into an uint32 value in microseconds
func ParseDuration(str string) (uint32, error) {
	if str == "" {
		return 0, nil
	}
	value, err := time.ParseDuration(str)
	if err != nil {
		return 0, err
	}
	if value < 0 {
		return 0, fmt.Errorf("duration value must be positive")
	}
	return uint32(value.Microseconds()), nil
}

// Parse a rate string into an uint64 value in bits per second
// e.g. 1000, 100kbit, 100Mbps, 1Gibps
func ParseRate(rate string) (uint64, error) {
	rate = strings.TrimSpace(strings.ToLower(rate))
	if rate == "" {
		return 0, nil
	}

	var unitMultiplier uint64 = 1
	if strings.HasSuffix(rate, "bit") {
		rate = strings.TrimSuffix(rate, "bit")
	} else if strings.HasSuffix(rate, "bps") {
		rate = strings.TrimSuffix(rate, "bps")
		unitMultiplier = 8
	}

	// Assume SI-prefixes by default
	var base uint64 = 1000
	// If using IEC-prefixes, switch to binary base, e.g. MiB
	if strings.HasSuffix(rate, "i") {
		rate = strings.TrimSuffix(rate, "i")
		base = 1024
	}

	for i, unit := range []string{"k", "m", "g", "t"} {
		if strings.HasSuffix(rate, unit) {
			rate = strings.TrimSuffix(rate, unit)
			for j := 0; j < i+1; j++ {
				unitMultiplier *= base
			}
			break
		}
	}

	value, err := strconv.ParseUint(rate, 10, 64)
	if err != nil {
		return 0, err
	}
	return value * unitMultiplier, nil
}

func SetVethQdiscs(veth *koko.VEth, qdiscs []netlink.Qdisc) (err error) {
	var vethNs ns.NetNS
	if veth.NsName == "" {
		if vethNs, err = ns.GetCurrentNS(); err != nil {
			log.Errorf("Failed to get current namespace: %v", err)
			return err
		}
	} else {
		if vethNs, err = ns.GetNS(veth.NsName); err != nil {
			log.Errorf("Failed to get namespace %s: %v", veth.NsName, err)
			return err
		}
	}
	defer vethNs.Close()

	err = ClearVethQdiscs(veth)
	if err != nil {
		log.Errorf("Failed to clear qdiscs on veth %s: %v", veth.LinkName, err)
	}

	return vethNs.Do(func(_ ns.NetNS) (err error) {
		var link netlink.Link
		if link, err = netlink.LinkByName(veth.LinkName); err != nil {
			log.Errorf("Cannot get link %s in namespace %s: %v", veth.LinkName, veth.NsName, err)
			return err
		}

		// If using netem, TBF will be its child, else TBF will be root
		withNetem := false
		for _, qdisc := range qdiscs {
			// Set link index and parent, assign handle
			var newQdisc netlink.Qdisc
			switch qdisc := qdisc.(type) {
			case *netlink.Netem:
				qdisc.LinkIndex = link.Attrs().Index
				qdisc.Parent = netlink.HANDLE_ROOT
				qdisc.Handle = netlink.MakeHandle(1, 0) // 1:0
				newQdisc = qdisc
				withNetem = true
				err = netlink.QdiscAdd(newQdisc)
			case *netlink.Tbf:
				// Set TBF qdisc via command since netlink has yet support setting burst size and latency
				args := []string{"qdisc", "add", "dev", veth.LinkName}
				if withNetem {
					args = append(args, "parent", "1:1", "handle", "10:0")
				} else {
					args = append(args, "root")
				}
				args = append(args, "tbf",
					"rate", fmt.Sprint(qdisc.Rate),
					// Use buffer as burst size since netlink has yet support setting burst size
					"burst", fmt.Sprint(qdisc.Buffer),
					"latency", "50ms",
					"minburst", fmt.Sprint(qdisc.Minburst),
				)
				cmd := exec.Command("tc", args...)
				output, _err := cmd.CombinedOutput()
				if _err != nil {
					log.Errorf("Failed to exec tc command '%s' (%s): %s", cmd.String(), _err, output)
					err = fmt.Errorf("(%s) %s", _err, output)
				}
				newQdisc = qdisc
			default:
				log.Errorf("Unsupported qdisc type %s", qdisc.Type())
			}

			log.Infof("Adding qdisc %v to link %s", qdisc, veth.LinkName)
			if err != nil {
				log.Errorf("Failed to add qdisc %v to link %s: %v", qdisc, veth.LinkName, err)
				return err
			}
		}
		return nil
	})
}

func ClearVethQdiscs(veth *koko.VEth) (err error) {
	var vethNs ns.NetNS
	if veth.NsName == "" {
		if vethNs, err = ns.GetCurrentNS(); err != nil {
			log.Errorf("Failed to get current namespace: %v", err)
			return err
		}
	} else {
		if vethNs, err = ns.GetNS(veth.NsName); err != nil {
			log.Errorf("Failed to get namespace %s: %v", veth.NsName, err)
			return err
		}
	}
	defer vethNs.Close()

	return vethNs.Do(func(_ ns.NetNS) (err error) {
		var link netlink.Link
		if link, err = netlink.LinkByName(veth.LinkName); err != nil {
			log.Errorf("Cannot get link %s in namespace %s: %v", veth.LinkName, veth.NsName, err)
			return err
		}
		qdiscs, err := netlink.QdiscList(link)
		if err != nil {
			log.Errorf("Failed to list qdiscs for link %s: %v", veth.LinkName, err)
			return err
		}
		for _, qdisc := range qdiscs {
			err = netlink.QdiscDel(qdisc)
			if err != nil {
				log.Errorf("Failed to delete qdisc %v from link %s: %v", qdisc, veth.LinkName, err)
			}
		}
		return nil
	})
}

// Calculate burst size for TBF qdisc
func getTbfBurst(rate uint64) uint32 {
	// At least Rate / Kernel HZ
	burst := uint32(rate / 250)
	// At least 5000 bytes
	if burst < 5000 {
		burst = 5000
	}
	log.Infof("TBF burst size: %d, rate: %d", burst, rate)
	return burst
}
