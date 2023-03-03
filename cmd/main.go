package main

import (
	"flag"
	"os"
	"strings"

	"github.com/goccy/go-yaml"
	koko "github.com/redhat-nfvpe/koko/api"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"github.com/y-young/kube-dtn/common"
	pb "github.com/y-young/kube-dtn/proto/v1"
)

type Query struct {
	Link     pb.Link `json:"link"`
	RemoteIP string  `json:"remote_ip"`
}

func (q *Query) Print() {
	log.Infof("Link: %s", q.Link.String())
	log.Infof("RemoteIP: %s", q.RemoteIP)
}

func main() {
	intf := flag.String("i", "", "Interface for vxlan")
	ip := flag.String("a", "", "Local IP for vxlan")
	path := flag.String("f", "", "Path to yaml file")
	flag.Parse()
	if *path == "" {
		log.Fatalf("Please provide a path to a yaml file")
		return
	}

	file, err := os.ReadFile(*path)
	if err != nil {
		log.Fatalf("Failed to read configuration: %s", err)
		return
	}

	var query Query
	err = yaml.Unmarshal(file, &query)
	if err != nil {
		log.Fatalf("Failed to parse configuration: %s", err)
		return
	}
	link := &query.Link
	query.Print()

	if *intf == "" || *ip == "" {
		*intf, *ip = getVxlanSource()
	}
	if *intf == "" || *ip == "" {
		log.Fatalf("Failed to get vxlan source, please specify manually")
		return
	}

	err = addLink(link, *intf, query.RemoteIP)
	if err != nil {
		log.Errorf("Failed to add link: %s", err)
	} else {
		log.Info("Successfully added link locally, now add this link to topology CRD")
	}
}

func getVxlanSource() (intf string, ip string) {
	links, _ := netlink.LinkList()
	for _, l := range links {
		attrs := l.Attrs()
		if strings.HasPrefix(attrs.Name, "eth") || strings.HasPrefix(attrs.Name, "ens") {
			intf = attrs.Name
			log.Infof("VXLAN Source Interface: %s", intf)
			addrs, _ := netlink.AddrList(l, netlink.FAMILY_V4)
			for _, a := range addrs {
				ip = a.IP.String()
				log.Infof("Local Address: %s", ip)
			}
			break
		}
	}
	return intf, ip
}

func addLink(link *pb.Link, srcIntf string, peerIP string) error {
	// Build koko's veth struct for local intf
	// We're connecting physical host interface, so use root network namespace
	myVeth, err := common.MakeVeth("", link.LocalIntf, link.LocalIp, "")
	if err != nil {
		return err
	}

	// Creating koko's Vxlan struct
	vxlan := common.MakeVxlan(srcIntf, peerIP, link.Uid)
	// Checking if interface already exists
	iExist, _ := koko.IsExistLinkInNS(myVeth.NsName, myVeth.LinkName)
	if iExist { // If VXLAN intf exists, we need to remove it first
		log.Infof("VXLAN intf already exists, removing it first")
		if err := myVeth.RemoveVethLink(); err != nil {
			log.Infof("Failed to remove a local stale VXLAN interface %s for pod %s", myVeth.LinkName, "local")
			return err
		}
	}
	if err = common.MakeVxLan(myVeth, vxlan, link); err != nil {
		log.Infof("Error when creating a Vxlan interface with koko: %s", err)
		return err
	}
	return nil
}
