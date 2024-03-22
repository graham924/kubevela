/*
Copyright 2021 The KubeVela Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	oamv1alpha2 "github.com/oam-dev/kubevela/pkg/controller/core.oam.dev/v1alpha2"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	injectorcontroller "github.com/oam-dev/trait-injector/controllers"
	"github.com/oam-dev/trait-injector/pkg/injector"
	"github.com/oam-dev/trait-injector/pkg/plugin"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	standardcontroller "github.com/oam-dev/kubevela/pkg/controller"
	oamcontroller "github.com/oam-dev/kubevela/pkg/controller/core.oam.dev"
	"github.com/oam-dev/kubevela/pkg/controller/utils"
	"github.com/oam-dev/kubevela/pkg/dsl/definition"
	"github.com/oam-dev/kubevela/pkg/oam"
	"github.com/oam-dev/kubevela/pkg/oam/discoverymapper"
	"github.com/oam-dev/kubevela/pkg/utils/common"
	"github.com/oam-dev/kubevela/pkg/utils/system"
	oamwebhook "github.com/oam-dev/kubevela/pkg/webhook/core.oam.dev"
	velawebhook "github.com/oam-dev/kubevela/pkg/webhook/standard.oam.dev"
	"github.com/oam-dev/kubevela/version"
)

const (
	// kubevelaName kubevela项目名称
	kubevelaName = "kubevela"
)

var (
	// setupLog 启动日志记录器
	setupLog = ctrl.Log.WithName(kubevelaName)
	// scheme kubevela的 公共scheme
	scheme             = common.Scheme
	waitSecretTimeout  = 90 * time.Second
	waitSecretInterval = 2 * time.Second
)

func main() {
	/* 命令行参数 */
	// 是否使用webhook、是否使用trait注入
	var useWebhook, useTraitInjector bool
	// 证书目录
	var certDir string
	// webhook端口
	var webhookPort int
	// metrics监控地址、日志文件、领导者选举命名空间
	var metricsAddr, logFilePath, leaderElectionNamespace string
	// 是否启动领导者选举、是否启动日志压缩、是否启动日志调试
	var enableLeaderElection, logCompress, logDebug bool
	// 日志保留天数
	var logRetainDate int
	// 控制器参数
	var controllerArgs oamcontroller.Args
	// 健康检查地址
	var healthAddr string
	// 要禁用的功能，使用逗号分隔(,)
	var disableCaps string
	// 存储驱动的路径
	var storageDriver string
	// 同步周期
	var syncPeriod time.Duration
	// 指示workload和trait是否只执行一次：如果没有spec的改变，只有其他改变的情况下，是否重新执行workload和trait
	var applyOnceOnly string

	flag.BoolVar(&useWebhook, "use-webhook", false, "Enable Admission Webhook")
	flag.BoolVar(&useTraitInjector, "use-trait-injector", false, "Enable TraitInjector")
	flag.StringVar(&certDir, "webhook-cert-dir", "/k8s-webhook-server/serving-certs", "Admission webhook cert/key dir.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "admission webhook listen address")
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&logFilePath, "log-file-path", "", "The file to write logs to.")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&leaderElectionNamespace, "leader-election-namespace", "",
		"Determines the namespace in which the leader election configmap will be created.")
	flag.IntVar(&logRetainDate, "log-retain-date", 7, "The number of days of logs history to retain.")
	flag.BoolVar(&logCompress, "log-compress", true, "Enable compression on the rotated logs.")
	flag.BoolVar(&logDebug, "log-debug", false, "Enable debug logs for development purpose")
	// controller args 的 内部参数
	flag.IntVar(&controllerArgs.RevisionLimit, "revision-limit", 50,
		"RevisionLimit is the maximum number of revisions that will be maintained. The default value is 50.")
	flag.StringVar(&controllerArgs.CustomRevisionHookURL, "custom-revision-hook-url", "",
		"custom-revision-hook-url is a webhook url which will let KubeVela core to call with applicationConfiguration and component info and return a customized component revision")
	flag.BoolVar(&controllerArgs.ApplicationConfigurationInstalled, "app-config-installed", true,
		"app-config-installed indicates if applicationConfiguration CRD is installed")
	flag.StringVar(&healthAddr, "health-addr", ":9440", "The address the health endpoint binds to.")
	flag.StringVar(&applyOnceOnly, "apply-once-only", "false",
		"For the purpose of some production environment that workload or trait should not be affected if no spec change, available options: on, off, force.")
	flag.StringVar(&disableCaps, "disable-caps", "", "To be disabled builtin capability list.")
	flag.StringVar(&storageDriver, "storage-driver", "Local", "Application file save to the storage driver")
	flag.DurationVar(&syncPeriod, "informer-re-sync-interval", 60*time.Minute,
		"controller shared informer lister full re-sync period")
	flag.StringVar(&oam.SystemDefinitonNamespace, "system-definition-namespace", "vela-system", "define the namespace of the system-level definition")
	flag.Parse()

	/* 设置日志相关 */
	// setup logging：先得到日志输出位置，再创建 logger
	var w io.Writer
	if len(logFilePath) > 0 {
		// 如果logFilePath有值，则将日志输出到指定的文件中：
		// - 文件名由logFilePath指定
		// - 日志文件的最大保留时间为logRetainDate天
		// - 并且根据logCompress的值决定是否压缩日志文件。
		w = zapcore.AddSync(&lumberjack.Logger{
			Filename: logFilePath,
			MaxAge:   logRetainDate, // days
			Compress: logCompress,
		})
	} else {
		// 如果logFilePath为空，则将日志输出到标准输出（控制台）中
		w = os.Stdout
	}
	// 使用zap.New()创建了一个logger，并传入一个匿名函数 来 配置zap.Options
	logger := zap.New(func(o *zap.Options) {
		// 是否以开发模式运行日志
		o.Development = logDebug
		// 指定日志输出的目的地
		o.DestWritter = w
	})
	// 将日志记录器设置给 controller-runtime 的全局日志记录器
	ctrl.SetLogger(logger)

	/* 打印 kubevela 系统信息 */
	// 打印kubevela版本信息、git版本信息、禁用功能信息、全局namespace信息
	setupLog.Info(fmt.Sprintf("KubeVela Version: %s, GIT Revision: %s.", version.VelaVersion, version.GitRevision))
	setupLog.Info(fmt.Sprintf("Disable Capabilities: %s.", disableCaps))
	setupLog.Info(fmt.Sprintf("core init with definition namespace %s", oam.SystemDefinitonNamespace))

	/* 加载kubernetes的连接信息，得到restConfig */
	// 尝试从环境变量、kubeconfig 文件或者其他默认方式来加载配置，并确保能够成功建立与 Kubernetes 集群的连接
	// 如果在任何情况下都无法成功获取到有效的配置，该函数会panic并导致程序终止运行，这就是函数名中“OrDie”部分的含义
	restConfig := ctrl.GetConfigOrDie()
	// 标识请求的来源
	restConfig.UserAgent = kubevelaName + "/" + version.GitRevision

	/* 创建一个manager对象 */
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                  scheme,
		MetricsBindAddress:      metricsAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionNamespace: leaderElectionNamespace,
		LeaderElectionID:        kubevelaName,
		Port:                    webhookPort,
		CertDir:                 certDir,
		HealthProbeBindAddress:  healthAddr,
		SyncPeriod:              &syncPeriod,
	})
	if err != nil {
		setupLog.Error(err, "unable to create a controller manager")
		os.Exit(1)
	}

	/* 注册健康检查 */
	if err := registerHealthChecks(mgr); err != nil {
		setupLog.Error(err, "unable to register ready/health checks")
		os.Exit(1)
	}

	/* 检查 功能禁用设置参数 的合法性：disableCaps的值只能是""、"all"、内置的几个功能名称 */
	if err := utils.CheckDisabledCapabilities(disableCaps); err != nil {
		setupLog.Error(err, "unable to get enabled capabilities")
		os.Exit(1)
	}

	/* 检查applyOnceOnly参数取值是否合法：只能是"on"、"空/false/off"、"force"，并将值设置给controllerArgs.ApplyMode */
	switch strings.ToLower(applyOnceOnly) {
	case "", "false", string(oamcontroller.ApplyOnceOnlyOff):
		controllerArgs.ApplyMode = oamcontroller.ApplyOnceOnlyOff
		setupLog.Info("ApplyOnceOnly is disabled")
	case "true", string(oamcontroller.ApplyOnceOnlyOn):
		controllerArgs.ApplyMode = oamcontroller.ApplyOnceOnlyOn
		setupLog.Info("ApplyOnceOnly is enabled, that means workload or trait only apply once if no spec change even they are changed by others")
	case string(oamcontroller.ApplyOnceOnlyForce):
		controllerArgs.ApplyMode = oamcontroller.ApplyOnceOnlyForce
		setupLog.Info("ApplyOnceOnlyForce is enabled, that means workload or trait only apply once if no spec change even they are changed or deleted by others")
	default:
		setupLog.Error(fmt.Errorf("invalid apply-once-only value: %s", applyOnceOnly),
			"unable to setup the vela core controller",
			"valid apply-once-only value:", "on/off/force, by default it's off")
		os.Exit(1)
	}

	/* 创建一个 CRD API资源 的发现客户端，设置给 controllerArgs.DiscoveryMapper */
	dm, err := discoverymapper.New(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "failed to create CRD discovery client")
		os.Exit(1)
	}
	controllerArgs.DiscoveryMapper = dm

	/* TODO 创建一个针对 CUE 包客户端的自定义资源发现（CRD discovery）实例，用于发现、管理或与 CUE 包相关的自定义资源 */
	pd, err := definition.NewPackageDiscover(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "failed to create CRD discovery for CUE package client")
		os.Exit(1)
	}
	controllerArgs.PackageDiscover = pd

	/* 注册所有CRD资源的 webhook */
	if useWebhook {
		setupLog.Info("vela webhook enabled, will serving at :" + strconv.Itoa(webhookPort))
		// core.oam.dev 包下所有的 webhook 注册到 manager 的 webhook server
		oamwebhook.Register(mgr, controllerArgs)
		// standard.oam.dev 包下所有的 webhook 注册到 manager 的 webhook server
		velawebhook.Register(mgr, disableCaps)
		// 等待webhook secret准备就绪，以避免mgr运行崩溃
		if err := waitWebhookSecretVolume(certDir, waitSecretTimeout, waitSecretInterval); err != nil {
			setupLog.Error(err, "unable to get webhook secret")
			os.Exit(1)
		}
	}

	/* 安装所有 core.oam.dev 包的 controller 到 manager 中*/
	if err = oamv1alpha2.Setup(mgr, controllerArgs, logging.NewLogrLogger(setupLog)); err != nil {
		setupLog.Error(err, "unable to setup the oam core controller")
		os.Exit(1)
	}

	/* 安装所有 standard.oam.dev 包的 controller 到 manager 中 */
	if err = standardcontroller.Setup(mgr, disableCaps); err != nil {
		setupLog.Error(err, "unable to setup the vela core controller")
		os.Exit(1)
	}

	/* 检查环境变量STORAGE_DRIVER，如果env存在，则跳过，不存在则将 storageDriver 参数设置到env */
	if driver := os.Getenv(system.StorageDriverEnv); len(driver) == 0 {
		// first use system environment,
		err := os.Setenv(system.StorageDriverEnv, storageDriver)
		if err != nil {
			setupLog.Error(err, "unable to setup the vela core controller")
			os.Exit(1)
		}
	}
	setupLog.Info("use storage driver", "storageDriver", os.Getenv(system.StorageDriverEnv))

	/* 根据useTraitInjector参数，选择是否开启 trait注入 功能 */
	if useTraitInjector {
		// 注册服务注入器（默认包括对 Deployment、Statefulset 的注入）
		plugin.RegisterTargetInjectors(injector.Defaults()...)

		// 创建一个ServiceBinding的controller
		tiWebhook := &injectorcontroller.ServiceBindingReconciler{
			Client:   mgr.GetClient(),
			Log:      ctrl.Log.WithName("controllers").WithName("ServiceBinding"),
			Scheme:   mgr.GetScheme(),
			Recorder: mgr.GetEventRecorderFor("servicebinding"),
		}

		// 将 ServiceBinding 的 controller 注册到 manager 中
		if err = (tiWebhook).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "ServiceBinding")
			os.Exit(1)
		}
		// this has hard coded requirement "./ssl/service-injector.pem", "./ssl/service-injector.key"
		// 启动了一个 HTTP 服务器，用于启动 ServiceBinding Admission Webhook 服务并监听请求的方法
		go tiWebhook.ServeAdmission()
	}

	/* 启动 manager */
	setupLog.Info("starting the vela controller manager")

	// 创建一个信号channel，用于监听进程退出信号，然后启动manager
	if err := mgr.Start(makeSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
	setupLog.Info("program safely stops...")
}

// registerHealthChecks is used to create readiness&liveness probes
func registerHealthChecks(mgr ctrl.Manager) error {
	setupLog.Info("creating readiness/health check")
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	// TODO: change the health check to be different from readiness check
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	return nil
}

// waitWebhookSecretVolume waits for webhook secret ready to avoid mgr running crash
// 等待webhook secret准备就绪，以避免mgr运行崩溃。
func waitWebhookSecretVolume(certDir string, timeout, interval time.Duration) error {
	start := time.Now()
	// 循环检测certDir目录是否存在
	for {
		time.Sleep(interval)
		// 如果超过设定的超时时间仍未找到，则返回超时错误
		if time.Since(start) > timeout {
			return fmt.Errorf("getting webhook secret timeout after %s", timeout.String())
		}
		// 如果目录存在，则进一步检查目录是否为空以及其中的secret文件是否为空
		setupLog.Info(fmt.Sprintf("waiting webhook secret, time consumed: %d/%d seconds ...",
			int64(time.Since(start).Seconds()), int64(timeout.Seconds())))
		if _, err := os.Stat(certDir); !os.IsNotExist(err) {
			ready := func() bool {
				f, err := os.Open(filepath.Clean(certDir))
				if err != nil {
					return false
				}
				// nolint
				defer f.Close()
				// check if dir is empty
				if _, err := f.Readdir(1); errors.Is(err, io.EOF) {
					return false
				}
				// check if secret files are empty
				err = filepath.Walk(certDir, func(path string, info os.FileInfo, err error) error {
					// even Cert dir is created, cert files are still empty for a while
					if info.Size() == 0 {
						return errors.New("secret is not ready")
					}
					return nil
				})
				if err == nil {
					setupLog.Info(fmt.Sprintf("webhook secret is ready (time consumed: %d seconds)",
						int64(time.Since(start).Seconds())))
					return true
				}
				return false
			}()
			if ready {
				return nil
			}
		}
	}
}

// makeSignalHandler 创建一个信号channel，用于接收操作系统信号
func makeSignalHandler() (stopCh <-chan struct{}) {
	stop := make(chan struct{})
	c := make(chan os.Signal, 2)

	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-c
		close(stop)

		// second signal. Exit directly.
		<-c
		os.Exit(1)
	}()

	return stop
}
