/*
Copyright 2019 The Crossplane Authors.

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

package v1alpha2

import (
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	ctrl "sigs.k8s.io/controller-runtime"

	controller "github.com/oam-dev/kubevela/pkg/controller/core.oam.dev"
	"github.com/oam-dev/kubevela/pkg/controller/core.oam.dev/v1alpha2/appdeployment"
	"github.com/oam-dev/kubevela/pkg/controller/core.oam.dev/v1alpha2/application"
	"github.com/oam-dev/kubevela/pkg/controller/core.oam.dev/v1alpha2/applicationconfiguration"
	"github.com/oam-dev/kubevela/pkg/controller/core.oam.dev/v1alpha2/applicationcontext"
	"github.com/oam-dev/kubevela/pkg/controller/core.oam.dev/v1alpha2/applicationrollout"
	"github.com/oam-dev/kubevela/pkg/controller/core.oam.dev/v1alpha2/core/components/componentdefinition"
	"github.com/oam-dev/kubevela/pkg/controller/core.oam.dev/v1alpha2/core/scopes/healthscope"
	"github.com/oam-dev/kubevela/pkg/controller/core.oam.dev/v1alpha2/core/traits/manualscalertrait"
	"github.com/oam-dev/kubevela/pkg/controller/core.oam.dev/v1alpha2/core/traits/traitdefinition"
	"github.com/oam-dev/kubevela/pkg/controller/core.oam.dev/v1alpha2/core/workloads/containerizedworkload"
)

// Setup workload controllers.
func Setup(mgr ctrl.Manager, args controller.Args, l logging.Logger) error {
	// 创建一个 func(ctrl.Manager, controller.Args, logging.Logger) error 类型的切片
	// 遍历切片中的每一个方法，依次调用
	// 每一个方法都是一个 controller 的 Setup 方法
	for _, setup := range []func(ctrl.Manager, controller.Args, logging.Logger) error{
		containerizedworkload.Setup, manualscalertrait.Setup, healthscope.Setup,
		application.Setup, applicationrollout.Setup, applicationcontext.Setup, appdeployment.Setup,
		traitdefinition.Setup, componentdefinition.Setup,
	} {
		if err := setup(mgr, args, l); err != nil {
			return err
		}
	}
	// 如果controller参数中 ApplicationConfigurationInstalled 为 true，则也安装 applicationconfiguration
	if args.ApplicationConfigurationInstalled {
		return applicationconfiguration.Setup(mgr, args, l)
	}
	return nil
}
