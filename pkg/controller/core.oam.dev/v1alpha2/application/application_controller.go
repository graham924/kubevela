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

package application

import (
	"context"
	"time"

	"github.com/crossplane/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/appfile"
	core "github.com/oam-dev/kubevela/pkg/controller/core.oam.dev"
	"github.com/oam-dev/kubevela/pkg/dsl/definition"
	"github.com/oam-dev/kubevela/pkg/oam/discoverymapper"
	oamutil "github.com/oam-dev/kubevela/pkg/oam/util"
	"github.com/oam-dev/kubevela/pkg/utils/apply"
)

// RolloutReconcileWaitTime is the time to wait before reconcile again an application still in rollout phase
const (
	RolloutReconcileWaitTime      = time.Second * 3
	resourceTrackerFinalizer      = "resourceTracker.finalizer.core.oam.dev"
	errUpdateApplicationStatus    = "cannot update application status"
	errUpdateApplicationFinalizer = "cannot update application finalizer"
)

// Reconciler reconciles a Application object
type Reconciler struct {
	// 实现了client.Client接口的对象，用于与Kubernetes API进行交互
	client.Client
	// dm 实现了discoverymapper.DiscoveryMapper接口的对象，用于发现Kubernetes集群中的资源
	dm discoverymapper.DiscoveryMapper
	// pd 指向definition.PackageDiscover的指针，用于发现和解析Kubernetes资源的定义
	pd *definition.PackageDiscover
	// Log 实现了logr.Logger接口的对象，用于记录日志信息
	Log logr.Logger
	// Scheme 资源注册表
	Scheme *runtime.Scheme
	// applicator 实现了apply.Applicator接口的对象，kubernetes资源的apply操作器
	applicator apply.Applicator
}

// 对 core.oam.dev.applications 及 applications/status 资源的操作权限
// +kubebuilder:rbac:groups=core.oam.dev,resources=applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.oam.dev,resources=applications/status,verbs=get;update;patch

// Reconcile process app event
func (r *Reconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	// 创建一个上下文对象
	ctx := context.Background()

	// 设置日志对象，使之携带了application的NamespacedName
	applog := r.Log.WithValues("application", req.NamespacedName)

	// 获取待调谐的application对象
	app := new(v1beta1.Application)
	if err := r.Get(ctx, client.ObjectKey{
		Name:      req.Name,
		Namespace: req.Namespace,
	}, app); err != nil {
		// 如果没找到，则认为该对象已经被删除了，返回后不会重试
		if kerrors.IsNotFound(err) {
			err = nil
		}
		return ctrl.Result{}, err
	}

	// 创建一个appHandler对象，其中包含了很多方法，用于处理application对象
	handler := &appHandler{
		r:      r,
		app:    app,
		logger: applog,
	}

	// 处理Kubernetes资源的finalizer，GC
	if app.ObjectMeta.DeletionTimestamp.IsZero() {
		// 如果 metadata.DeletionTimestamp 为0，表示该对象不需要删除
		// 该对象不需要删除，则尝试为其添加finalizer，以备后续删除的时候gc使用
		if registerFinalizers(app) {
			// 如果添加成功，说明是第一次进入调谐逻辑，也就是刚刚创建application的时候
			// 刚创建，无需进行下面的调谐逻辑，直接使用Client，更新application对象即可
			applog.Info("Register new finalizer", "application", app.Namespace+"/"+app.Name, "finalizers", app.ObjectMeta.Finalizers)
			// 如果 r.Client.Update(ctx, app) 报错，errors.Wrap会捕获后，添加errUpdateApplicationFinalizer的描述信息，返回上层；
			// 如果不报错，则errors.Wrap返回nil
			return reconcile.Result{}, errors.Wrap(r.Client.Update(ctx, app), errUpdateApplicationFinalizer)
		}
	} else {
		// 如果 metadata.DeletionTimestamp 不为0，表示该对象已经进入删除阶段了
		// 处理 该application的所有追踪资源，删除所有的ResourceTracker后，把application对象的finalizer也移除
		needUpdate, err := handler.removeResourceTracker(ctx)
		if err != nil {
			applog.Error(err, "Failed to remove application resourceTracker")

			// 如果处理 ResourceTracker 和 finalizer 过程中出错，则更新application.status的condition，添加一个 ReconcileError 调谐错误的信息
			app.Status.SetConditions(v1alpha1.ReconcileError(errors.Wrap(err, "error to  remove finalizer")))

			// 如果 r.UpdateStatus(ctx, app) 报错，errors.Wrap会捕获后，添加errUpdateApplicationStatus的描述信息，返回上层；
			// 如果不报错，则errors.Wrap返回nil
			return reconcile.Result{}, errors.Wrap(r.UpdateStatus(ctx, app), errUpdateApplicationStatus)
		}
		// 如果所有ResourceTracker都移除成功，并且finalizer也都清除，则直接更新资源，kubernetes会自动删除application对象
		if needUpdate {
			applog.Info("remove finalizer of application", "application", app.Namespace+"/"+app.Name, "finalizers", app.ObjectMeta.Finalizers)

			// 如果 r.Update(ctx, app) 报错，errors.Wrap会捕获后，添加errUpdateApplicationFinalizer的描述信息，返回上层；
			// 如果不报错，则errors.Wrap返回nil
			return ctrl.Result{}, errors.Wrap(r.Update(ctx, app), errUpdateApplicationFinalizer)
		}
		// deleting and no need to handle finalizer
		// 如果 ResourceTracker 和 finalizer 移除失败，但此时metadata.DeletionTimestamp 不为0，application应该是只读状态，因此直接返回nil
		return reconcile.Result{}, nil
	}

	// 进入application的渲染阶段
	applog.Info("Start Rendering")

	// 设置app的状态为 Rendering 渲染中
	app.Status.Phase = common.ApplicationRendering

	// 开始 parse template
	applog.Info("parse template")

	// 创建一个appParser对象，用于解析application的模板
	appParser := appfile.NewApplicationParser(r.Client, r.dm, r.pd)

	// 将 application 的 ns，设置到context中
	ctx = oamutil.SetNamespaceInCtx(ctx, app.Namespace)

	// 根据application的模板，解析并生成 Appfile
	generatedAppfile, err := appParser.GenerateAppFile(ctx, app.Name, app)
	if err != nil {
		applog.Error(err, "[Handle Parse]")
		// 解析失败，向application的status中添加一个解析错误的condition
		app.Status.SetConditions(errorCondition("Parsed", err))
		// 处理错误公共逻辑
		return handler.handleErr(err)
	}

	// 解析成功，向application的status中添加一个解析成功的condition
	app.Status.SetConditions(readyCondition("Parsed"))

	// 将解析得到的appfile，设置到 handler
	handler.appfile = generatedAppfile

	// 根据 handler 中的 application 和 appfile，生成一个修订资源 ApplicationRevision
	appRev, err := handler.GenerateAppRevision(ctx)
	if err != nil {
		// 如果生成失败，则在application的status中添加一个解析错误的condition
		applog.Error(err, "[Handle Calculate Revision]")
		app.Status.SetConditions(errorCondition("Parsed", err))
		// 处理错误公共逻辑
		return handler.handleErr(err)
	}

	// Record the revision so it can be used to render data in context.appRevision
	// 生成了修订资源 ApplicationRevision，将修订资源名称，设置到 appfile.RevisionName
	generatedAppfile.RevisionName = appRev.Name

	applog.Info("build template")
	// build template to applicationconfig & component
	ac, comps, err := appParser.GenerateApplicationConfiguration(generatedAppfile, app.Namespace)
	if err != nil {
		applog.Error(err, "[Handle GenerateApplicationConfiguration]")
		app.Status.SetConditions(errorCondition("Built", err))
		return handler.handleErr(err)
	}

	err = handler.handleResourceTracker(ctx, comps, ac)
	if err != nil {
		applog.Error(err, "[Handle resourceTracker]")
		return handler.handleErr(err)
	}

	// pass the App label and annotation to ac except some app specific ones
	oamutil.PassLabelAndAnnotation(app, ac)

	app.Status.SetConditions(readyCondition("Built"))
	applog.Info("apply application revision & component to the cluster")
	// apply application revision & component to the cluster
	if err := handler.apply(ctx, appRev, ac, comps); err != nil {
		applog.Error(err, "[Handle apply]")
		app.Status.SetConditions(errorCondition("Applied", err))
		return handler.handleErr(err)
	}

	app.Status.SetConditions(readyCondition("Applied"))

	// 进入application的健康检查阶段
	app.Status.Phase = common.ApplicationHealthChecking
	applog.Info("check application health status")
	// check application health status
	appCompStatus, healthy, err := handler.statusAggregate(generatedAppfile)
	if err != nil {
		applog.Error(err, "[status aggregate]")
		app.Status.SetConditions(errorCondition("HealthCheck", err))
		return handler.handleErr(err)
	}
	if !healthy {
		app.Status.SetConditions(errorCondition("HealthCheck", errors.New("not healthy")))

		app.Status.Services = appCompStatus
		// unhealthy will check again after 10s
		return ctrl.Result{RequeueAfter: time.Second * 10}, r.Status().Update(ctx, app)
	}
	app.Status.Services = appCompStatus
	app.Status.SetConditions(readyCondition("HealthCheck"))

	// 设置application的状态为Running，并更新status
	app.Status.Phase = common.ApplicationRunning
	// Gather status of components
	var refComps []v1alpha1.TypedReference
	for _, comp := range comps {
		refComps = append(refComps, v1alpha1.TypedReference{
			APIVersion: comp.APIVersion,
			Kind:       comp.Kind,
			Name:       comp.Name,
			UID:        app.UID,
		})
	}
	app.Status.Components = refComps
	return ctrl.Result{}, r.UpdateStatus(ctx, app)
}

// if any finalizers newly registered, return true
// registerFinalizers 为Application注册finalizers，返回注册结果
func registerFinalizers(app *v1beta1.Application) bool {
	// 如果当前对象，metadata.finalizers 中，还没有resourceTrackerFinalizer标记，并且 application 有需要追踪的对象
	// 则添加resourceTrackerFinalizer
	if !meta.FinalizerExists(&app.ObjectMeta, resourceTrackerFinalizer) && app.Status.ResourceTracker != nil {
		// 添加 resourceTrackerFinalizer 标记
		meta.AddFinalizer(&app.ObjectMeta, resourceTrackerFinalizer)
		// 只要添加成功，就返回true
		return true
	}
	// 如果不需要添加finalizers，则返回false
	return false
}

// UpdateStatus updates v1beta1.Application's Status with retry.RetryOnConflict
// 更新app的status
func (r *Reconciler) UpdateStatus(ctx context.Context, app *v1beta1.Application, opts ...client.UpdateOption) error {
	// 深拷贝获取应用的状态
	status := app.DeepCopy().Status
	// 使用重试机制来更新应用状态，使用了默认的退避策略，当更新发生冲突时会进行重试
	return retry.RetryOnConflict(retry.DefaultBackoff, func() (err error) {
		// 从kubernetes环境中获取最新的app对象，覆盖app
		if err = r.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: app.Name}, app); err != nil {
			return
		}
		// 将更改后的status，覆盖app的status
		app.Status = status
		// 更新环境中app.status
		return r.Status().Update(ctx, app, opts...)
	})
}

/* manager启动时，安装 application controller */
// Setup adds a controller that reconciles AppRollout.
func Setup(mgr ctrl.Manager, args core.Args, _ logging.Logger) error {
	// 创建一个 Reconciler 实例
	reconciler := Reconciler{
		Client:     mgr.GetClient(),
		Log:        ctrl.Log.WithName("Application"),
		Scheme:     mgr.GetScheme(),
		dm:         args.DiscoveryMapper,
		pd:         args.PackageDiscover,
		applicator: apply.NewAPIApplicator(mgr.GetClient()),
	}
	return reconciler.setupWithManager(mgr)
}

// setupWithManager 安装application controller核心逻辑
func (r *Reconciler) setupWithManager(mgr ctrl.Manager) error {
	// If Application Own these two child objects, AC status change will notify application controller and recursively update AC again, and trigger application event again...
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Application{}).
		Complete(r)
}
