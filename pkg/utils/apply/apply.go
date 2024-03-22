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

package apply

import (
	"context"

	"github.com/pkg/errors"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/pkg/oam"
)

/* kubernetes 资源的更新器接口，以apply的方式更新资源，类似使用kubectl apply的方式 */

// Applicator applies new state to an object or create it if not exist.
// It uses the same mechanism as `kubectl apply`, that is, for each resource being applied,
// computing a three-way diff merge in client side based on its current state, modified stated,
// and last-applied-state which is tracked through an specific annotation.
// If the resource doesn't exist before, Apply will create it.
type Applicator interface {
	Apply(context.Context, runtime.Object, ...ApplyOption) error
}

// ApplyOption is called before applying state to the object.
// ApplyOption is still called even if the object does NOT exist.
// If the object does not exist, `existing` will be assigned as `nil`.
// nolint: golint
type ApplyOption func(ctx context.Context, existing, desired runtime.Object) error

/* kubernetes 资源的 创建器 接口，包含一个 创建或获取资源 的方法 */
type creator interface {
	// createOrGetExisting 根据ApplyOption，判断kubernetes是否已经存在该资源，不存在则创建，存在则get后返回该资源
	createOrGetExisting(context.Context, client.Client, runtime.Object, ...ApplyOption) (runtime.Object, error)
}

type creatorFn func(context.Context, client.Client, runtime.Object, ...ApplyOption) (runtime.Object, error)

func (fn creatorFn) createOrGetExisting(ctx context.Context, c client.Client, o runtime.Object, ao ...ApplyOption) (runtime.Object, error) {
	return fn(ctx, c, o, ao...)
}

/* kubernetes 资源的 合并器 接口，包含一个 合并新配置到资源 的方法 */
type patcher interface {
	patch(c, m runtime.Object) (client.Patch, error)
}

type patcherFn func(c, m runtime.Object) (client.Patch, error)

func (fn patcherFn) patch(c, m runtime.Object) (client.Patch, error) {
	return fn(c, m)
}

/* APIApplicator：kubernetes资源的修改器，实现了Applicator接口，并内置一个 creator 和 patcher 对象 */

// APIApplicator implements Applicator
type APIApplicator struct {
	creator
	patcher
	c client.Client
}

// NewAPIApplicator creates an Applicator that applies state to an
// object or creates the object if not exist.
// 创建一个 kubernetes 资源的更新器
func NewAPIApplicator(c client.Client) *APIApplicator {
	return &APIApplicator{
		// 给 creatorFn 类，赋上实际的方法
		creator: creatorFn(createOrGetExisting),
		// 给 patcherFn 类，赋上实际的方法
		patcher: patcherFn(threeWayMergePatch),
		c:       c,
	}
}

// Apply applies new state to an object or create it if not exist
func (a *APIApplicator) Apply(ctx context.Context, desired runtime.Object, ao ...ApplyOption) error {
	// 调用creator的方法，根据ApplyOption，判断kubernetes是否已经存在该资源，不存在则创建，存在则get后返回该资源
	existing, err := a.createOrGetExisting(ctx, a.c, desired, ao...)
	if err != nil {
		return err
	}
	if existing == nil {
		return nil
	}

	// the object already exists, apply new state
	// 依次执行aos的每一个ApplyOption，ApplyOption具体干什么，就需要调用方传入了
	if err := executeApplyOptions(ctx, existing, desired, ao); err != nil {
		return err
	}

	// 调用patcher的方法，将desired和existing进行合并，并更新kubernetes中的该资源
	loggingApply("patching object", desired)
	patch, err := a.patcher.patch(existing, desired)
	if err != nil {
		return errors.Wrap(err, "cannot calculate patch by computing a three way diff")
	}
	return errors.Wrapf(a.c.Patch(ctx, desired, patch), "cannot patch object")
}

// loggingApply will record a log with desired object applied
func loggingApply(msg string, desired runtime.Object) {
	d, ok := desired.(metav1.Object)
	if !ok {
		klog.InfoS(msg, "resource", desired.GetObjectKind().GroupVersionKind().String())
		return
	}
	klog.InfoS(msg, "name", d.GetName(), "resource", desired.GetObjectKind().GroupVersionKind().String())
}

// createOrGetExisting 赋给APIApplicator的creator，根据ApplyOption，判断kubernetes是否已经存在该资源，不存在则创建，存在则get后返回该资源

// createOrGetExisting will create the object if it does not exist
// or get and return the existing object
func createOrGetExisting(ctx context.Context, c client.Client, desired runtime.Object, ao ...ApplyOption) (runtime.Object, error) {
	m, ok := desired.(oam.Object)
	if !ok {
		return nil, errors.New("cannot access object metadata")
	}

	var create = func() (runtime.Object, error) {
		// execute ApplyOptions even the object doesn't exist
		if err := executeApplyOptions(ctx, nil, desired, ao); err != nil {
			return nil, err
		}
		if err := addLastAppliedConfigAnnotation(desired); err != nil {
			return nil, err
		}
		loggingApply("creating object", desired)
		return nil, errors.Wrap(c.Create(ctx, desired), "cannot create object")
	}

	// allow to create object with only generateName
	if m.GetName() == "" && m.GetGenerateName() != "" {
		return create()
	}

	existing := &unstructured.Unstructured{}
	existing.GetObjectKind().SetGroupVersionKind(desired.GetObjectKind().GroupVersionKind())
	err := c.Get(ctx, types.NamespacedName{Name: m.GetName(), Namespace: m.GetNamespace()}, existing)
	if kerrors.IsNotFound(err) {
		return create()
	}
	if err != nil {
		return nil, errors.Wrap(err, "cannot get object")
	}
	return existing, nil
}

// executeApplyOptions 依次执行aos的每一个ApplyOption，ApplyOption具体干什么，就需要调用方传入了
func executeApplyOptions(ctx context.Context, existing, desired runtime.Object, aos []ApplyOption) error {
	// if existing is nil, it means the object is going to be created.
	// ApplyOption function should handle this situation carefully by itself.
	for _, fn := range aos {
		if err := fn(ctx, existing, desired); err != nil {
			return errors.Wrap(err, "cannot apply ApplyOption")
		}
	}
	return nil
}

// MustBeControllableBy requires that the new object is controllable by an
// object with the supplied UID. An object is controllable if its controller
// reference includes the supplied UID.
func MustBeControllableBy(u types.UID) ApplyOption {
	return func(_ context.Context, existing, _ runtime.Object) error {
		if existing == nil {
			return nil
		}
		c := metav1.GetControllerOf(existing.(metav1.Object))
		if c == nil {
			return nil
		}

		if c.UID != u {
			return errors.Errorf("existing object is not controlled by UID %q", u)
		}
		return nil
	}
}
