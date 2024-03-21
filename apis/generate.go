//go:build generate
// +build generate

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

// See the below link for details on what is happening here.
// https://github.com/golang/go/wiki/Modules#how-can-i-track-tool-dependencies-for-a-module

// NOTE(@wonderflow) We don't remove existing CRDs here, because the crd folders contain not only auto generated.

// Generate deepcopy methodsets and CRD manifests

// 下面包含多个go:generate指令，用于在编译时执行生成代码的命令

// 生成符合K8s v1版本CRD规范的Manifest文件。它调用controller-gen工具，指定使用headerFile定义的头部文件，处理./...路径下的所有文件，并将生成的配置保存到../config/crd/base目录中
//go:generate go run -tags generate sigs.k8s.io/controller-tools/cmd/controller-gen object:headerFile=../hack/boilerplate.go.txt paths=./... crd:crdVersions=v1 output:artifacts:config=../config/crd/base

// Generate legacy_support for K8s 1.12~1.15 versions CRD manifests
// 生成兼容K8s 1.12~1.15版本的CRD Manifests。它同样调用controller-gen工具，但使用trivialVersions=true来指定生成简化版本的CRD，生成的配置保存到../legacy/charts/vela-core-legacy/crds目录中。随后，运行../legacy/convert/main.go脚本处理这些CRD文件
//go:generate go run -tags generate sigs.k8s.io/controller-tools/cmd/controller-gen object:headerFile=../hack/boilerplate.go.txt paths=./... crd:trivialVersions=true output:artifacts:config=../legacy/charts/vela-core-legacy/crds
//go:generate go run ../legacy/convert/main.go ../legacy/charts/vela-core-legacy/crds

// 更新../charts/vela-core/crds/standard.oam.dev_podspecworkloads.yaml文件。它调用../hack/crd/update.go脚本来执行更新操作。
//go:generate go run ../hack/crd/update.go ../charts/vela-core/crds/standard.oam.dev_podspecworkloads.yaml

package apis

import (
	_ "sigs.k8s.io/controller-tools/cmd/controller-gen" //nolint:typecheck
)
