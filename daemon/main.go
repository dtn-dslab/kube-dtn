package main

import (
	"flag"
	"os"
	"strconv"

	log "github.com/sirupsen/logrus"
	"github.com/y-young/kube-dtn/daemon/cni"
	"github.com/y-young/kube-dtn/daemon/grpcwire"
	"github.com/y-young/kube-dtn/daemon/kubedtn"
	"github.com/y-young/kube-dtn/daemon/vxlan"
)

const (
	defaultPort = 51111
)

func main() {

	if err := cni.Init(); err != nil {
		log.Errorf("Failed to initialise CNI plugin: %v", err)
		os.Exit(1)
	}
	defer cni.Cleanup()

	isDebug := flag.Bool("d", false, "enable degugging")
	grpcPort, err := strconv.Atoi(os.Getenv("GRPC_PORT"))
	if err != nil || grpcPort == 0 {
		grpcPort = defaultPort
	}
	flag.Parse()
	log.SetLevel(log.InfoLevel)
	if *isDebug {
		log.SetLevel(log.DebugLevel)
		log.Debug("Verbose logging enabled")
	}

	kubedtn.InitLogger()
	grpcwire.InitLogger()
	vxlan.InitLogger()

	m, err := kubedtn.New(kubedtn.Config{
		Port: grpcPort,
	})
	if err != nil {
		log.Errorf("Failed to create kubedtn: %v", err)
		os.Exit(1)
	}
	log.Info("Starting kubedtn daemon...with grpc support")

	if err := m.Serve(); err != nil {
		log.Errorf("Daemon exited badly: %v", err)
		os.Exit(1)
	}
}
