package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"gpu-adapter/internal/adapter"
	"gpu-adapter/internal/host"
	"gpu-adapter/internal/nvidia"
	"gpu-adapter/internal/server"
)

func main() {
	pflag.String("config", "", "config file (YAML, JSON or TOML) with the keys below")
	pflag.String("api-token", "", "Bearer token that callers must send")
	pflag.String("node-name", "", "name of the node this adapter manages")
	pflag.String("pod-namespace", "", "namespace for the TIME_SLICING record ConfigMap")
	pflag.String("nvml-library", "", "path of libnvidia-ml.so.1; empty uses the default search path")
	pflag.String("dra-plugin-namespace", "gpu-operator", "namespace of the DRA kubelet plugin Pods")
	pflag.String("dra-plugin-selector", "app=nvidia-dra-driver-kubelet-plugin", "label selector of the DRA kubelet plugin Pods")
	pflag.String("listen-addr", ":8080", "API listen address")
	pflag.String("log-level", "info", "debug, info, warn or error")
	pflag.Parse()

	cfg := viper.New()
	cfg.SetEnvPrefix("GPU_ADAPTER")
	cfg.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	cfg.AutomaticEnv()
	if err := cfg.BindPFlags(pflag.CommandLine); err != nil {
		panic(err)
	}
	if path := cfg.GetString("config"); path != "" {
		cfg.SetConfigFile(path)
		if err := cfg.ReadInConfig(); err != nil {
			panic(err)
		}
	}

	logConfig := zap.NewProductionConfig()
	logConfig.Sampling = nil
	if err := logConfig.Level.UnmarshalText([]byte(cfg.GetString("log-level"))); err != nil {
		panic(err)
	}
	log := zap.Must(logConfig.Build())
	zap.ReplaceGlobals(log)

	for _, key := range []string{"api-token", "node-name", "pod-namespace"} {
		if cfg.GetString(key) == "" {
			log.Fatal(key + " is required")
		}
	}
	token := cfg.GetString("api-token")
	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal("cannot load in-cluster Kubernetes config", zap.Error(err))
	}
	a := &adapter.Adapter{
		Kube:            kubernetes.NewForConfigOrDie(kubeConfig),
		Dynamic:         dynamic.NewForConfigOrDie(kubeConfig),
		Vendor:          &nvidia.NVML{Lib: nvml.New(nvml.WithLibraryPath(cfg.GetString("nvml-library")))},
		Host:            host.Host{Sys: "/sys", Proc: "/proc", Grace: 10 * time.Second, PCIVendor: nvidia.PCIVendor, DeviceMajor: nvidia.DeviceMajor, Keep: nvidia.Daemons},
		Node:            cfg.GetString("node-name"),
		Namespace:       cfg.GetString("pod-namespace"),
		PluginNamespace: cfg.GetString("dra-plugin-namespace"),
		PluginSelector:  cfg.GetString("dra-plugin-selector"),
	}
	handler, err := server.Handler(a, token)
	if err != nil {
		log.Fatal("cannot load the embedded OpenAPI spec", zap.Error(err))
	}
	srv := &http.Server{Addr: cfg.GetString("listen-addr"), Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	log.Info("listening", zap.String("addr", srv.Addr), zap.String("node", a.Node))
	stop, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer cancel()
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("server stopped", zap.Error(err))
		}
	}()
	<-stop.Done()
	log.Info("shutting down after requests in progress finish")
	_ = srv.Shutdown(context.Background())
}
