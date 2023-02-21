package main

import (
	"flag"
	"net/http"
	"os"
	"strconv"

	log "github.com/sirupsen/logrus"
	"github.com/y-young/kube-dtn/common"
	"github.com/y-young/kube-dtn/daemon/cni"
	"github.com/y-young/kube-dtn/daemon/grpcwire"
	"github.com/y-young/kube-dtn/daemon/kubedtn"
	"github.com/y-young/kube-dtn/daemon/metrics"
	"github.com/y-young/kube-dtn/daemon/vxlan"

	"github.com/prometheus/client_golang/prometheus/promhttp"
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
		grpcPort, err = strconv.Atoi(common.DefaultPort)
		if err != nil {
			panic("Failed to parse default port")
		}
	}

	httpAddr := os.Getenv("HTTP_ADDR")
	if err != nil || httpAddr == "" {
		httpAddr = common.HttpAddr
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

	topologyManager := metrics.NewTopologyManager()
	reg := metrics.NewRegistry(topologyManager)

	go func() {
		log.Infof("HTTP server listening on port %s", httpAddr)
		http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		http.ListenAndServe(httpAddr, nil)
	}()

	m, err := kubedtn.New(kubedtn.Config{
		Port: grpcPort,
	}, topologyManager)
	if err != nil {
		log.Errorf("Failed to create kubedtn: %v", err)
		os.Exit(1)
	}
	log.Info("Starting kubedtn daemon...with grpc support")
	log.Infof("GRPC listening on port %d", grpcPort)

	if err := m.Serve(); err != nil {
		log.Errorf("Daemon exited badly: %v", err)
		os.Exit(1)
	}
}
