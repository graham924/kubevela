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

package controller

import (
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/oam-dev/kubevela/pkg/controller/common"
	"github.com/oam-dev/kubevela/pkg/controller/standard.oam.dev/v1alpha1/podspecworkload"
	"github.com/oam-dev/kubevela/pkg/controller/utils"
)

// Setup workload controllers.
func Setup(mgr ctrl.Manager, disableCaps string) error {
	// 新建一个 func(ctrl.Manager) error 类型切片
	var functions []func(ctrl.Manager) error

	// 根据 禁用功能的配置参数，将要启用的功能添加到 functions 切片中
	switch disableCaps {
	case common.DisableNoneCaps:
		// 如果没有要禁用的，则将所有功能添加到 functions 中（实际上只有 podspecworkload 一个）
		functions = []func(ctrl.Manager) error{
			podspecworkload.Setup,
		}
	case common.DisableAllCaps:
		// 如果所有都禁用，则什么都不用添加
	default:
		// 将 禁用功能的参数disableCaps 切割 成一个 set
		disableCapsSet := utils.StoreInSet(disableCaps)
		// 如果没有禁用 podspecworkload，则添加 podspecworkload 的 controller setup
		if !disableCapsSet.Contains(common.PodspecWorkloadControllerName) {
			functions = append(functions, podspecworkload.Setup)
		}
	}

	// 遍历所有的 functions，依次执行
	for _, setup := range functions {
		if err := setup(mgr); err != nil {
			return err
		}
	}
	return nil
}
